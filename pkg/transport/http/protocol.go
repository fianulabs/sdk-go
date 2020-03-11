package http

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cloudevents/sdk-go/pkg/binding"
	cecontext "github.com/cloudevents/sdk-go/pkg/context"
)

const (
	// DefaultShutdownTimeout defines the default timeout given to the http.Server when calling Shutdown.
	DefaultShutdownTimeout = time.Minute * 1
)

// Protocol acts as both a http client and a http handler.
type Protocol struct {
	Target          *url.URL
	RequestTemplate *http.Request
	transformers    binding.TransformerFactories
	Client          *http.Client
	incoming        chan msgErr
}

func NewProtocol(opts ...ProtocolOption) (*Protocol, error) {
	p := &Protocol{
		transformers: make(binding.TransformerFactories, 0),
		incoming:     make(chan msgErr),
	}
	if err := p.applyOptions(opts...); err != nil {
		return nil, err
	}

	if p.Client == nil {
		p.Client = http.DefaultClient
	}

	return p, nil
}

func (p *Protocol) applyOptions(opts ...ProtocolOption) error {
	for _, fn := range opts {
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

// Send implements binding.Sender
func (p *Protocol) Send(ctx context.Context, m binding.Message) error {
	_, err := p.Request(ctx, m)
	return err
}

// Request implements binding.Requester
func (p *Protocol) Request(ctx context.Context, m binding.Message) (binding.Message, error) {
	var err error
	defer func() { _ = m.Finish(err) }()

	req := p.makeRequest(ctx)

	if p.Client == nil || req == nil || req.URL == nil {
		return nil, fmt.Errorf("not initialized: %#v", p)
	}

	if err = WriteHttpRequest(ctx, m, req, p.transformers); err != nil {
		return nil, err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	return NewMessage(resp.Header, resp.Body), nil
}

func (p *Protocol) makeRequest(ctx context.Context) *http.Request {
	// TODO: support custom headers from context?
	req := &http.Request{
		Header: make(http.Header),
		// TODO: HeaderFrom(ctx),
	}

	if p.RequestTemplate != nil {
		req.Method = p.RequestTemplate.Method
		req.URL = p.RequestTemplate.URL
		req.Close = p.RequestTemplate.Close
		req.Host = p.RequestTemplate.Host
		copyHeadersEnsure(p.RequestTemplate.Header, &req.Header)
	}

	if p.Target != nil {
		req.URL = p.Target
	}

	// Override the default request with target from context.
	if target := cecontext.TargetFrom(ctx); target != nil {
		req.URL = target
	}
	return req.WithContext(ctx)
}

// Ensure to is a non-nil map before copying
func copyHeadersEnsure(from http.Header, to *http.Header) {
	if len(from) > 0 {
		if *to == nil {
			*to = http.Header{}
		}
		copyHeaders(from, *to)
	}
}

func copyHeaders(from, to http.Header) {
	if from == nil || to == nil {
		return
	}
	for header, values := range from {
		for _, value := range values {
			to.Add(header, value)
		}
	}
}

// Receive the next incoming HTTP request as a CloudEvent.
// Returns non-nil error if the incoming HTTP request fails to parse as a CloudEvent
// Returns io.EOF if the receiver is closed.
func (p *Protocol) Receive(ctx context.Context) (binding.Message, error) {
	msgErr, ok := <-p.incoming
	if !ok {
		return nil, io.EOF
	}
	return msgErr.msg, msgErr.err
}

type msgErr struct {
	msg *Message
	err error
}

// ServeHTTP implements http.Handler.
// Blocks until Message.Finish is called.
func (p *Protocol) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	var err error
	m := NewMessageFromHttpRequest(req)
	if m.ReadEncoding() == binding.EncodingUnknown {
		p.incoming <- msgErr{nil, binding.ErrUnknownEncoding}
	}
	done := make(chan error)
	m.OnFinish = func(err error) error {
		if m.resp != nil {
			err := WriteHttpResponseWriter(context.Background(), m.resp, rw, p.transformers)
			_ = m.resp.Finish(err)
		}
		m.resp = nil
		done <- err
		return nil
	}
	p.incoming <- msgErr{m, err} // Send to Receive()
	if err = <-done; err != nil {
		http.Error(rw, fmt.Sprintf("cannot forward CloudEvent: %v", err), http.StatusInternalServerError)
	}
}
