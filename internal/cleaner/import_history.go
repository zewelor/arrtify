package cleaner

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type importHistoryRecord struct {
	ID         *int              `json:"id"`
	EventType  *string           `json:"eventType"`
	DownloadID *string           `json:"downloadId"`
	MovieID    *int              `json:"movieId"`
	SeriesID   *int              `json:"seriesId"`
	EpisodeID  *int              `json:"episodeId"`
	Data       map[string]string `json:"data"`
	Hash       string            `json:"-"`
}

func (record importHistoryRecord) fileID() (int, error) {
	id, err := strconv.Atoi(record.Data["fileId"])
	if err != nil || id <= 0 {
		return 0, errors.New("invalid import file identity")
	}
	return id, nil
}

func importedHashes(torrents []Torrent, label yamlLabel) map[string]bool {
	hashes := make(map[string]bool)
	if label == "" {
		return hashes
	}
	for _, torrent := range torrents {
		if slices.ContainsFunc(torrent.Labels, func(value string) bool {
			return strings.EqualFold(value, string(label))
		}) {
			hashes[torrent.Hash] = true
		}
	}
	return hashes
}

func readImportHistory(ctx context.Context, reader arrReader, visit func(importHistoryRecord) error) error {
	query := url.Values{"sortKey": {"date"}, "sortDirection": {"descending"}}
	return reader.pages(ctx, "history", query, func(raw json.RawMessage) (int, error) {
		var record importHistoryRecord
		if json.Unmarshal(raw, &record) != nil || record.ID == nil || record.EventType == nil {
			return 0, errors.New("invalid history record")
		}
		if *record.EventType != "downloadFolderImported" || record.DownloadID == nil {
			return *record.ID, nil
		}
		record.Hash = strings.ToLower(*record.DownloadID)
		return *record.ID, visit(record)
	})
}
