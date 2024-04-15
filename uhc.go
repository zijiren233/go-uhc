package uhc

import (
	"net/http"
)

var DefaultClient = &http.Client{
	Transport: DefaultTransport,
}

func NewClient(opts ...TransportOption) *http.Client {
	return &http.Client{
		Transport: NewTransport(opts...),
	}
}

func Do(req *http.Request) (*http.Response, error) {
	return DefaultClient.Do(req)
}

func DoWithOptions(req *http.Request, opts ...TransportOption) (*http.Response, error) {
	return NewClient(opts...).Do(req)
}
