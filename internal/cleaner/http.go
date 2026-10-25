package cleaner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	maxResponseBytes = 16 << 20
	apiHTTPTimeout   = 10 * time.Second
)

type apiHTTPResponse struct {
	status int
	header http.Header
	body   []byte
}

func newReadOnlyHTTPClient() *http.Client {
	return &http.Client{
		Timeout: apiHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func doAPIRequest(ctx context.Context, method, target, operation string, headers http.Header, body []byte, basicUser, basicPassword string) (apiHTTPResponse, error) {
	var result apiHTTPResponse
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return result, errors.New(operation + ": invalid request")
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	if basicUser != "" || basicPassword != "" {
		req.SetBasicAuth(basicUser, basicPassword)
	}

	resp, err := newReadOnlyHTTPClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return result, fmt.Errorf("%s: %w", operation, ctx.Err())
		}
		return result, errors.New(operation + ": request failed")
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return result, fmt.Errorf("%s: %w", operation, ctx.Err())
		}
		return result, errors.New(operation + ": response read failed")
	}
	if len(responseBody) > maxResponseBytes {
		return result, errors.New(operation + ": response exceeds 16 MiB")
	}
	return apiHTTPResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: responseBody}, nil
}
