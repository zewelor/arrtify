package cleaner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	queuePageSize   = 100
	maxQueueRecords = 100000
)

type queuePage struct {
	Page          *int               `json:"page"`
	PageSize      *int               `json:"pageSize"`
	SortKey       *string            `json:"sortKey"`
	SortDirection *string            `json:"sortDirection"`
	TotalRecords  *int               `json:"totalRecords"`
	Records       *[]json.RawMessage `json:"records"`
}

type queueMetadata struct {
	pageSize      int
	sortKey       string
	sortDirection string
	totalRecords  int
}

func ReadQueue(ctx context.Context, endpoint Endpoint, apiKey, service string) (map[string]bool, error) {
	if service != "sonarr" && service != "radarr" {
		return nil, errors.New("queue: unsupported service")
	}
	if strings.TrimSpace(apiKey) == "" || strings.ContainsAny(apiKey, "\r\n") {
		return nil, fmt.Errorf("%s queue: API key missing or invalid", service)
	}
	u, err := queueURL(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%s queue: invalid endpoint", service)
	}
	unknownFlag := "includeUnknownSeriesItems"
	if service == "radarr" {
		unknownFlag = "includeUnknownMovieItems"
	}
	ids := make(map[string]bool)
	seenRecordIDs := make(map[int]struct{})
	var expectedMetadata queueMetadata

	for page := 1; ; page++ {
		query := url.Values{}
		query.Set("page", fmt.Sprintf("%d", page))
		query.Set("pageSize", fmt.Sprintf("%d", queuePageSize))
		query.Set(unknownFlag, "true")
		pageURL := *u
		pageURL.RawQuery = query.Encode()
		headers := make(http.Header)
		headers.Set("X-Api-Key", apiKey)
		response, err := doAPIRequest(ctx, http.MethodGet, pageURL.String(), service+" queue", headers, nil, "", "")
		if err != nil {
			return nil, err
		}
		if response.status != http.StatusOK {
			return nil, fmt.Errorf("%s queue: HTTP status %d", service, response.status)
		}
		var responsePage queuePage
		if err := json.Unmarshal(response.body, &responsePage); err != nil {
			return nil, fmt.Errorf("%s queue: invalid response JSON", service)
		}
		if responsePage.Page == nil || responsePage.PageSize == nil || responsePage.SortKey == nil || responsePage.SortDirection == nil || responsePage.TotalRecords == nil || responsePage.Records == nil {
			return nil, fmt.Errorf("%s queue: missing or null pagination field", service)
		}
		if *responsePage.Page != page || *responsePage.PageSize != queuePageSize || *responsePage.TotalRecords < 0 || *responsePage.TotalRecords > maxQueueRecords || strings.TrimSpace(*responsePage.SortKey) == "" || (*responsePage.SortDirection != "ascending" && *responsePage.SortDirection != "descending") {
			return nil, fmt.Errorf("%s queue: invalid pagination metadata", service)
		}
		metadata := queueMetadata{
			pageSize:      *responsePage.PageSize,
			sortKey:       *responsePage.SortKey,
			sortDirection: *responsePage.SortDirection,
			totalRecords:  *responsePage.TotalRecords,
		}
		if page == 1 {
			expectedMetadata = metadata
		} else if metadata != expectedMetadata {
			return nil, fmt.Errorf("%s queue: pagination metadata changed between pages", service)
		}

		remaining := expectedMetadata.totalRecords - (page-1)*queuePageSize
		if remaining < 0 {
			return nil, fmt.Errorf("%s queue: unexpected page beyond total records", service)
		}
		wantRecords := remaining
		if wantRecords > queuePageSize {
			wantRecords = queuePageSize
		}
		if len(*responsePage.Records) != wantRecords {
			return nil, fmt.Errorf("%s queue: short or overfull page", service)
		}
		for _, rawRecord := range *responsePage.Records {
			recordID, downloadID, err := queueRecordIdentity(rawRecord)
			if err != nil {
				return nil, fmt.Errorf("%s queue: invalid queue record", service)
			}
			if _, duplicate := seenRecordIDs[recordID]; duplicate {
				return nil, fmt.Errorf("%s queue: duplicate record id", service)
			}
			seenRecordIDs[recordID] = struct{}{}
			if downloadID == "" {
				continue
			}
			normalized := strings.ToLower(downloadID)
			if strings.TrimSpace(normalized) != normalized {
				return nil, fmt.Errorf("%s queue: invalid downloadId", service)
			}
			ids[normalized] = true
		}
		if page*queuePageSize >= expectedMetadata.totalRecords {
			return ids, nil
		}
	}
}

func queueURL(endpoint Endpoint) (*url.URL, error) {
	u, err := parseEndpoint(endpoint.URL, false)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v3/queue"
	u.RawPath = ""
	return u, nil
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
