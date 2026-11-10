package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrProtocol = errors.New("api_failed")
var ErrCapacity = errors.New("capacity_exceeded")

type HTTPGateway struct {
	base      string
	client    *http.Client
	transport *http.Transport
}

func NewHTTPGateway(address string) *HTTPGateway {
	host, port, _ := net.SplitHostPort(address)
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true, MaxResponseHeaderBytes: 64 << 10, MaxIdleConnsPerHost: 1, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	return &HTTPGateway{base: "http://" + net.JoinHostPort(host, port), transport: transport, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (g *HTTPGateway) Close() { g.transport.CloseIdleConnections() }
func (g *HTTPGateway) Request(ctx context.Context, method string, t Target, body any) (Response, error) {
	result := Response{}
	route := "/apis/" + t.APIVersion
	if !strings.Contains(t.APIVersion, "/") {
		route = "/api/" + t.APIVersion
	}
	if t.Namespace != "" {
		route += "/namespaces/" + url.PathEscape(t.Namespace)
	}
	route += "/" + t.Resource.Plural
	if method != "POST" && t.Name != "" {
		route += "/" + url.PathEscape(t.Name)
	}
	var input io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return result, ErrProtocol
		}
		input = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, g.base+route, input)
	if e != nil {
		return result, ErrProtocol
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, e := g.client.Do(req)
	if e != nil {
		return result, e
	}
	defer res.Body.Close()
	result.Code = res.StatusCode
	ids := res.Header.Values("X-Dynamo-Request-ID")
	if len(ids) != 1 || len(ids[0]) == 0 || len(ids[0]) > 128 {
		return result, ErrProtocol
	}
	for _, b := range []byte(ids[0]) {
		if b < 33 || b > 126 {
			return result, ErrProtocol
		}
	}
	result.RequestID = ids[0]
	media, _, e := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if e != nil || media != "application/json" || res.Header.Get("Content-Encoding") != "" {
		return result, ErrProtocol
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (32<<20)+1))
	if e != nil {
		return result, e
	}
	if len(b) > 32<<20 {
		return result, ErrCapacity
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if e = dec.Decode(&result.Object); e != nil || result.Object == nil {
		return result, ErrProtocol
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return result, ErrProtocol
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		expectedKind := t.Kind
		expectedVersion := t.APIVersion
		if method == http.MethodDelete {
			expectedKind = "Status"
			expectedVersion = "v1"
		} else if method == http.MethodGet && t.Name == "" {
			expectedKind = t.Resource.ListKind
			if expectedKind == "" {
				expectedKind = t.Kind + "List"
			}
		}
		if str(result.Object["kind"]) != expectedKind || str(result.Object["apiVersion"]) != expectedVersion {
			return result, ErrProtocol
		}
	}
	return result, nil
}
