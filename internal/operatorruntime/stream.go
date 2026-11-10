package operatorruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
)

func (d *Docker) Stream(ctx context.Context, method, path string, body io.Reader, contentType string) (io.ReadCloser, error) {
	req, e := http.NewRequestWithContext(ctx, method, "http://docker/v1.47"+path, body)
	if e != nil {
		return nil, errors.New("invalid Docker stream request")
	}
	req.Header.Set("Content-Type", contentType)
	resp, e := d.client.Do(req)
	if e != nil {
		return nil, errors.New("Docker stream unavailable or cancelled")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &HTTPError{Method: method, Status: resp.StatusCode}
	}
	return resp.Body, nil
}
