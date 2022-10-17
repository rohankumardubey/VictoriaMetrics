package remoteread

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

var bodyBufferPool bytesutil.ByteBufferPool

const (
	defaultReadTimeout = 30 * time.Second
	remoteReadPath     = "/api/v1/read"
	healthPath         = "/health"
)

// StreamCallback is a callback function for processing time series
type StreamCallback func(series prompb.TimeSeries) error

// Client is an HTTP client for reading
// time series via remote read protocol.
type Client struct {
	addr     string
	c        *http.Client
	user     string
	password string
}

// Config is config for remote read.
type Config struct {
	// Addr of remote storage
	Addr string
	// ReadTimeout defines timeout for HTTP write request
	// to remote storage
	ReadTimeout time.Duration
	// Username is the remote read username, optional.
	Username string
	// Password is the remote read password, optional.
	Password string

	transport *http.Transport
}

// Filter is used for request remote read data by filter
type Filter struct {
	Min, Max   int64
	Label      string
	LabelValue string
}

// NewClient returns client for
// reading time series via remote read protocol.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("config.Addr can't be empty")
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = defaultReadTimeout
	}

	c := &Client{
		c: &http.Client{
			Timeout:   cfg.ReadTimeout,
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
		},
		addr:     strings.TrimSuffix(cfg.Addr, "/"),
		user:     cfg.Username,
		password: cfg.Password,
	}
	return c, nil
}

// Read fetch data from remote read source
func (c *Client) Read(ctx context.Context, filter *Filter, streamCb StreamCallback) error {
	query, err := c.query(filter)
	if err != nil {
		return fmt.Errorf("error prepare stream query: %w", err)
	}
	req := &prompb.ReadRequest{
		Queries: []*prompb.Query{
			query,
		},
		AcceptedResponseTypes: []prompb.ReadRequest_ResponseType{prompb.ReadRequest_STREAMED_XOR_CHUNKS},
	}
	data, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("unable to marshal read request: %w", err)
	}

	const attempts = 5
	b := snappy.Encode(nil, data)
	for i := 0; i < attempts; i++ {
		err := c.fetch(ctx, b, streamCb)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return fmt.Errorf("process stoped")
			}
			logger.Errorf("attempt %d to fetch data from remote storage: %s", i+1, err)
			// sleeping to avoid remote db hammering
			time.Sleep(time.Second)
			continue
		}
		return nil
	}
	return nil
}

// Ping checks the health of the read source
func (c *Client) Ping() error {
	url := c.addr + healthPath
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("cannot create request to %q: %s", url, err)
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status code: %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) fetch(ctx context.Context, data []byte, streamCb StreamCallback) error {
	r := bytes.NewReader(data)
	url := c.addr + remoteReadPath
	req, err := http.NewRequest("POST", url, r)
	if err != nil {
		return fmt.Errorf("failed to create new HTTP request: %w", err)
	}

	req.Header.Add("Content-Encoding", "snappy")
	req.Header.Add("Accept-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Read-Version", "0.1.0")

	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}

	resp, err := c.c.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("error while sending request to %s: %w; Data len %d(%d)",
			req.URL.Redacted(), err, len(data), r.Size())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected response code %d for %s. Response body %q",
			resp.StatusCode, req.URL.Redacted(), body)
	}
	defer func() { _ = resp.Body.Close() }()

	bb := bodyBufferPool.Get()
	defer bodyBufferPool.Put(bb)

	stream := remote.NewChunkedReader(resp.Body, remote.DefaultChunkedReadLimit, bb.B)

	for {
		var ts prompb.TimeSeries
		res := &prompb.ChunkedReadResponse{}
		err := stream.NextProto(res)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		for _, series := range res.ChunkedSeries {
			ts.Labels = append(ts.Labels, series.Labels...)
			for _, chunk := range series.Chunks {
				samples, err := parseSamples(chunk.Data)
				if err != nil {
					return err
				}
				ts.Samples = append(ts.Samples, samples...)
			}
			if err := streamCb(ts); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Client) query(filter *Filter) (*prompb.Query, error) {
	var ms *labels.Matcher
	if filter.Label == "" && filter.LabelValue == "" {
		ms = labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+")
	} else {
		ms = labels.MustNewMatcher(labels.MatchRegexp, filter.Label, filter.LabelValue)
	}
	m, err := toLabelMatchers(ms)
	if err != nil {
		return nil, err
	}
	return &prompb.Query{
		StartTimestampMs: filter.Min,
		EndTimestampMs:   filter.Max - 1,
		Matchers:         []*prompb.LabelMatcher{m},
	}, nil
}

func toLabelMatchers(matcher *labels.Matcher) (*prompb.LabelMatcher, error) {
	var mType prompb.LabelMatcher_Type
	switch matcher.Type {
	case labels.MatchEqual:
		mType = prompb.LabelMatcher_EQ
	case labels.MatchNotEqual:
		mType = prompb.LabelMatcher_NEQ
	case labels.MatchRegexp:
		mType = prompb.LabelMatcher_RE
	case labels.MatchNotRegexp:
		mType = prompb.LabelMatcher_NRE
	default:
		return nil, fmt.Errorf("invalid matcher type")
	}
	return &prompb.LabelMatcher{
		Type:  mType,
		Name:  matcher.Name,
		Value: matcher.Value,
	}, nil
}

func parseSamples(chunk []byte) ([]prompb.Sample, error) {
	c, err := chunkenc.FromData(chunkenc.EncXOR, chunk)
	if err != nil {
		return nil, fmt.Errorf("error read chunk: %w", err)
	}

	var samples []prompb.Sample
	it := c.Iterator(nil)
	for it.Next() {
		if it.Err() != nil {
			return nil, fmt.Errorf("error iterate over chunks: %w", it.Err())
		}

		ts, v := it.At()
		s := prompb.Sample{
			Timestamp: ts,
			Value:     v,
		}
		samples = append(samples, s)
	}

	return samples, it.Err()
}