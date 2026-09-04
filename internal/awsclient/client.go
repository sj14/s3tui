// Package awsclient builds the AWS S3 clients from a config profile.
package awsclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/middleware"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/dustin/go-humanize"
	"github.com/sj14/s3tui/internal/config"
	"golang.org/x/time/rate"
)

// Client holds the base S3 client and creates region specific clients for
// buckets which do not live in the configured region.
type Client struct {
	base    *s3.Client
	awsCfg  aws.Config
	options []func(*s3.Options)

	// custom endpoints are not region routed
	fixedEndpoint bool

	mu            sync.Mutex
	regionClients map[string]*s3.Client
	bucketRegions map[string]string
}

// New creates the S3 client for the given profile.
func New(ctx context.Context, profile config.Profile, userAgent string) (*Client, error) {
	awsCfg := aws.Config{Region: profile.Region}

	switch {
	case profile.AccessKey != "" || profile.SecretKey != "":
		awsCfg.Credentials = aws.NewCredentialsCache(
			credentials.NewStaticCredentialsProvider(profile.AccessKey, profile.SecretKey, ""),
		)
	default:
		// Fall back to the usual AWS sources (env, shared config, SSO, ...).
		defaultCfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("loading aws default config: %w", err)
		}
		awsCfg.Credentials = defaultCfg.Credentials
		if awsCfg.Region == "" {
			awsCfg.Region = defaultCfg.Region
		}
	}

	if awsCfg.Region == "" {
		awsCfg.Region = "us-east-1"
	}

	if profile.Endpoint != "" {
		awsCfg.BaseEndpoint = aws.String(profile.Endpoint)
	}

	if profile.ReadOnly {
		awsCfg.RetryMaxAttempts = 1
	}

	httpClient, err := newHTTPClient(profile)
	if err != nil {
		return nil, err
	}

	options := []func(*s3.Options){
		func(o *s3.Options) {
			o.UsePathStyle = profile.PathStyle
			o.HTTPClient = httpClient
			o.APIOptions = append(o.APIOptions,
				middleware.AddUserAgentKeyValue("s3tui", userAgent),
			)
		},
	}

	return &Client{
		base:          s3.NewFromConfig(awsCfg, options...),
		awsCfg:        awsCfg,
		options:       options,
		fixedEndpoint: profile.Endpoint != "",
		regionClients: map[string]*s3.Client{},
		bucketRegions: map[string]string{},
	}, nil
}

// Base returns the client for the configured region.
func (c *Client) Base() *s3.Client {
	return c.base
}

// Region returns the configured region.
func (c *Client) Region() string {
	return c.awsCfg.Region
}

// SetBucketRegion caches the region of a bucket, e.g. from a ListBuckets result.
func (c *Client) SetBucketRegion(bucket, region string) {
	if bucket == "" || region == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.bucketRegions[bucket] = region
}

// ForBucket returns the client which is able to talk to the given bucket. On
// AWS, buckets outside of the configured region need a client of their own,
// otherwise the requests are answered with a redirect.
func (c *Client) ForBucket(ctx context.Context, bucket string) (*s3.Client, error) {
	if c.fixedEndpoint {
		return c.base, nil
	}

	c.mu.Lock()
	region, known := c.bucketRegions[bucket]
	c.mu.Unlock()

	if !known {
		resp, err := c.base.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(bucket)})
		if err != nil {
			// Not allowed to ask? Give the base client a try.
			return c.base, nil //nolint:nilerr
		}

		region = string(resp.LocationConstraint)
		if region == "" {
			region = "us-east-1" // the empty constraint means us-east-1
		}
		c.SetBucketRegion(bucket, region)
	}

	return c.forRegion(region), nil
}

func (c *Client) forRegion(region string) *s3.Client {
	if region == "" || region == c.awsCfg.Region {
		return c.base
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if client, ok := c.regionClients[region]; ok {
		return client
	}

	cfg := c.awsCfg
	cfg.Region = region

	client := s3.NewFromConfig(cfg, c.options...)
	c.regionClients[region] = client

	return client
}

func newHTTPClient(profile config.Profile) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if profile.Insecure {
		transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // requested by config
	}
	if profile.SNI != "" {
		transport.TLSClientConfig.ServerName = profile.SNI
	}
	if profile.Network != "" {
		transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, profile.Network, addr)
		}
	}

	wrapper := &transportWrapper{
		base:     transport,
		readOnly: profile.ReadOnly,
	}

	if profile.Bandwidth != "" {
		bandwidth, err := humanize.ParseBytes(profile.Bandwidth)
		if err != nil {
			return nil, fmt.Errorf("parsing bandwidth %q: %w", profile.Bandwidth, err)
		}

		wrapper.limiter = rate.NewLimiter(
			rate.Limit(bandwidth),
			64*1024, // add a small burst, otherwise it might fail
		)
	}

	return &http.Client{Transport: wrapper}, nil
}

type transportWrapper struct {
	base     http.RoundTripper
	readOnly bool
	limiter  *rate.Limiter
}

func (t *transportWrapper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.readOnly {
		switch req.Method {
		case http.MethodHead, http.MethodGet, http.MethodOptions, http.MethodTrace:
		default:
			return nil, fmt.Errorf("blocked by read-only mode")
		}
	}

	// What goes up counts against the same budget as what comes down: one
	// limiter, one line. A RoundTripper must not change the request it is
	// handed, so the body is swapped on a copy of it.
	if t.limiter != nil && req.Body != nil && req.Body != http.NoBody {
		limited := *req
		limited.Body = newRateLimitedBody(req.Context(), req.Body, t.limiter)
		req = &limited
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	if resp.Body != nil && t.limiter != nil {
		resp.Body = io.NopCloser(newRateLimitedReader(req.Context(), resp.Body, t.limiter))
	}

	return resp, nil
}
