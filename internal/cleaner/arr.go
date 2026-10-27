package cleaner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	arrPageSize   = 100
	maxArrRecords = 100000
)

type arrPage struct {
	Page          *int               `json:"page"`
	PageSize      *int               `json:"pageSize"`
	SortKey       *string            `json:"sortKey"`
	SortDirection *string            `json:"sortDirection"`
	TotalRecords  *int               `json:"totalRecords"`
	Records       *[]json.RawMessage `json:"records"`
}

type arrMetadata struct {
	pageSize      int
	sortKey       string
	sortDirection string
	totalRecords  int
}

type arrReader struct {
	endpoint Endpoint
	apiKey   string
	service  string
}

func (r arrReader) get(ctx context.Context, resource string, query url.Values) ([]byte, error) {
	operation := r.service + " " + resource
	if strings.TrimSpace(r.apiKey) == "" || strings.ContainsAny(r.apiKey, "\r\n") {
		return nil, fmt.Errorf("%s: API key missing or invalid", operation)
	}
	u, err := parseEndpoint(r.endpoint.URL, false)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid endpoint", operation)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v3/" + resource
	u.RawPath = ""
	u.RawQuery = query.Encode()
	headers := make(http.Header)
	headers.Set("X-Api-Key", r.apiKey)
	response, err := doAPIRequest(ctx, http.MethodGet, u.String(), operation, headers, nil, "", "")
	if err != nil {
		return nil, err
	}
	if response.status != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP status %d", operation, response.status)
	}
	return response.body, nil
}

// pages shares bounded pagination and consistency checks between queues and history.
func (r arrReader) pages(
	ctx context.Context,
	resource string,
	query url.Values,
	visit func(json.RawMessage) (int, error),
) error {
	operation := r.service + " " + resource
	seen := make(map[int]struct{})
	var expected arrMetadata
	for page := 1; ; page++ {
		query.Set("page", strconv.Itoa(page))
		query.Set("pageSize", strconv.Itoa(arrPageSize))
		body, err := r.get(ctx, resource, query)
		if err != nil {
			return err
		}
		var response arrPage
		if json.Unmarshal(body, &response) != nil {
			return fmt.Errorf("%s: invalid response JSON", operation)
		}
		missingPosition := response.Page == nil || response.PageSize == nil || response.TotalRecords == nil
		missingContent := response.SortKey == nil || response.SortDirection == nil || response.Records == nil
		if missingPosition || missingContent {
			return fmt.Errorf("%s: missing or null pagination field", operation)
		}
		invalidPosition := *response.Page != page || *response.PageSize != arrPageSize
		invalidTotal := *response.TotalRecords < 0 || *response.TotalRecords > maxArrRecords
		validDirection := *response.SortDirection == "ascending" || *response.SortDirection == "descending"
		invalidSort := strings.TrimSpace(*response.SortKey) == "" || !validDirection
		if invalidPosition || invalidTotal || invalidSort {
			return fmt.Errorf("%s: invalid pagination metadata", operation)
		}
		metadata := arrMetadata{
			pageSize: *response.PageSize, sortKey: *response.SortKey,
			sortDirection: *response.SortDirection, totalRecords: *response.TotalRecords,
		}
		if page == 1 {
			expected = metadata
		} else if metadata != expected {
			return fmt.Errorf("%s: pagination metadata changed between pages", operation)
		}
		remaining := expected.totalRecords - (page-1)*arrPageSize
		if remaining < 0 || len(*response.Records) != min(remaining, arrPageSize) {
			return fmt.Errorf("%s: short or overfull page", operation)
		}
		for _, raw := range *response.Records {
			id, err := visit(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", operation, err)
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("%s: duplicate record id", operation)
			}
			seen[id] = struct{}{}
		}
		if page*arrPageSize >= expected.totalRecords {
			return nil
		}
	}
}

func ReadQueue(ctx context.Context, endpoint Endpoint, apiKey, service string) (map[string]bool, error) {
	if service != "sonarr" && service != "radarr" {
		return nil, errors.New("queue: unsupported service")
	}
	unknownFlag := "includeUnknownSeriesItems"
	if service == "radarr" {
		unknownFlag = "includeUnknownMovieItems"
	}
	ids := make(map[string]bool)
	reader := arrReader{endpoint: endpoint, apiKey: apiKey, service: service}
	err := reader.pages(ctx, "queue", url.Values{unknownFlag: {"true"}}, func(raw json.RawMessage) (int, error) {
		recordID, downloadID, err := queueRecordIdentity(raw)
		if err != nil {
			return 0, errors.New("invalid queue record")
		}
		if strings.TrimSpace(downloadID) != downloadID {
			return 0, errors.New("invalid downloadId")
		}
		if downloadID != "" {
			ids[strings.ToLower(downloadID)] = true
		}
		return recordID, nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func queueRecordIdentity(raw json.RawMessage) (int, string, error) {
	var record struct {
		ID         *int            `json:"id"`
		DownloadID json.RawMessage `json:"downloadId"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return 0, "", err
	}
	if record.ID == nil || len(record.DownloadID) == 0 {
		return 0, "", errors.New("record id or downloadId missing or null")
	}
	value := bytes.TrimSpace(record.DownloadID)
	if bytes.Equal(value, []byte("null")) {
		return *record.ID, "", nil
	}
	var id string
	if err := json.Unmarshal(value, &id); err != nil {
		return 0, "", errors.New("downloadId must be string or null")
	}
	return *record.ID, id, nil
}
