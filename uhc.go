package uhc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

var _ io.ReadCloser = (*utlsHttpBody)(nil)

type utlsHttpBody struct {
	*utls.UConn
	rawBody io.ReadCloser
}

func (u *utlsHttpBody) Read(p []byte) (n int, err error) {
	return u.rawBody.Read(p)
}

func (u *utlsHttpBody) Close() error {
	defer u.UConn.Close()
	return u.rawBody.Close()
}

type UtlsHttpOption func(*UtlsHttpRoundTripper)

func WithHttpClient(client *http.Client) UtlsHttpOption {
	return func(options *UtlsHttpRoundTripper) {
		if client != nil {
			options.Timeout = client.Timeout
			options.Base = client.Transport
		}
	}
}

func WithContext(ctx context.Context) UtlsHttpOption {
	return func(options *UtlsHttpRoundTripper) {
		options.Ctx = ctx
	}
}

func WithBaseTransport(base http.RoundTripper) UtlsHttpOption {
	return func(options *UtlsHttpRoundTripper) {
		options.Base = base
	}
}

var DefaultUtlsHttpRoundTripper = NewUtlsHttpRoundTripper()

func NewUtlsHttpRoundTripper(opts ...UtlsHttpOption) *UtlsHttpRoundTripper {
	rt := &UtlsHttpRoundTripper{}
	for _, opt := range opts {
		opt(rt)
	}
	return rt
}

func Do(req *http.Request) (*http.Response, error) {
	return DefaultUtlsHttpRoundTripper.Do(req)
}

func DoWithOptions(req *http.Request, opts ...UtlsHttpOption) (*http.Response, error) {
	return NewUtlsHttpRoundTripper(opts...).Do(req)
}

type UtlsHttpRoundTripper struct {
	Base        http.RoundTripper
	Ctx         context.Context
	Timeout     time.Duration
	ProxySocks5 *url.URL
}

func (u *UtlsHttpRoundTripper) Do(req *http.Request) (*http.Response, error) {
	return u.RoundTrip(req)
}

func (u *UtlsHttpRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if u.Ctx == nil {
		u.Ctx = context.Background()
	}

	config := utls.Config{ServerName: req.URL.Hostname()}

	port := req.URL.Port()
	if port == "" {
		switch req.URL.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	var (
		dialConn net.Conn
		err      error
	)
	if u.ProxySocks5 != nil {
		var d proxy.Dialer
		d, err = proxy.FromURL(u.ProxySocks5, proxy.Direct)
		if err != nil {
			return nil, err
		}
		dialConn, err = d.Dial("tcp", fmt.Sprintf("%s:%s", req.URL.Hostname(), port))
	} else {
		dialConn, err = proxy.FromEnvironment().Dial("tcp", fmt.Sprintf("%s:%s", req.URL.Hostname(), port))
	}
	if err != nil {
		return nil, fmt.Errorf("dial failed: %w", err)
	}
	uTlsConn := utls.UClient(dialConn, &config, utls.HelloChrome_Auto)
	err = uTlsConn.HandshakeContext(u.Ctx)
	if err != nil {
		return nil, fmt.Errorf("utls handshake failed: %w", err)
	}
	resp, err := doHttpOverConn(u.Base, req, uTlsConn, uTlsConn.ConnectionState().NegotiatedProtocol)
	if err != nil {
		_ = uTlsConn.Close()
		return nil, fmt.Errorf("do http over conn failed: %w", err)
	}
	resp.Body = &utlsHttpBody{uTlsConn, resp.Body}
	return resp, nil
}

func getH2RoundTripper(rt http.RoundTripper, conn net.Conn) (http.RoundTripper, error) {
	if rt != nil {
		tr, ok := rt.(*http.Transport)
		if ok {
			tr = tr.Clone()
			tr.MaxResponseHeaderBytes = 262144
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return conn, nil
			}
			h2transport, err := http2.ConfigureTransports(tr)
			if err != nil {
				return nil, err
			}
			c, err := h2transport.NewClientConn(conn)
			if err != nil {
				return nil, err
			}
			return c, nil
		} else if h2tr, ok := rt.(*http2.Transport); ok {
			h2tr.MaxHeaderListSize = 262144
			c, err := h2tr.NewClientConn(conn)
			if err != nil {
				return nil, err
			}
			return c, nil
		} else {
			return nil, fmt.Errorf("unsupported RoundTripper: %T", rt)
		}
	} else {
		tr := http2.Transport{MaxHeaderListSize: 262144}
		c, err := tr.NewClientConn(conn)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

func doHttpOverConn(rt http.RoundTripper, req *http.Request, conn net.Conn, alpn string) (*http.Response, error) {
	switch alpn {
	case "h2":
		req.Proto = "HTTP/2.0"
		req.ProtoMajor = 2
		req.ProtoMinor = 0
		rt, err := getH2RoundTripper(rt, conn)
		if err != nil {
			return nil, fmt.Errorf("get http/2 round tripper failed: %w", err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			return nil, fmt.Errorf("do http/2 request failed: %w", err)
		}
		return resp, nil
	case "http/1.1", "":
		req.Proto = "HTTP/1.1"
		req.ProtoMajor = 1
		req.ProtoMinor = 1
		err := req.Write(conn)
		if err != nil {
			return nil, fmt.Errorf("get http/1.1 round tripper failed: %w", err)
		}
		return http.ReadResponse(bufio.NewReader(conn), req)
	default:
		return nil, fmt.Errorf("unsupported ALPN: %v", alpn)
	}
}
