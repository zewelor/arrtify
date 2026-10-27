package cleaner

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
)

// SonarrImportDecision describes the known episode imports, not filesystem links.
type SonarrImportDecision struct {
	State            string `json:"state"`
	Reason           string `json:"reason"`
	ImportedEpisodes int    `json:"imported_episodes"`
}

type sonarrImportRef struct {
	seriesID  int
	episodeID int
	fileID    int
}

func readSonarrImports(ctx context.Context, c Config, torrents []Torrent) (map[string]*SonarrImportDecision, error) {
	assessments := make(map[string]*SonarrImportDecision)
	for hash := range importedHashes(torrents, c.Sonarr.ImportedLabel) {
		assessments[hash] = &SonarrImportDecision{State: "unknown", Reason: "no_import_history"}
	}
	if len(assessments) == 0 {
		return assessments, nil
	}
	reader := arrReader{endpoint: c.Sonarr.Endpoint, apiKey: c.Credentials.SonarrKey, service: "sonarr"}
	imports := make(map[string]map[sonarrImportRef]struct{})
	series := make(map[int]bool)
	err := readImportHistory(ctx, reader, func(record importHistoryRecord) error {
		if assessments[record.Hash] == nil {
			return nil
		}
		fileID, err := record.fileID()
		invalidSeries := record.SeriesID == nil || *record.SeriesID <= 0
		invalidEpisode := record.EpisodeID == nil || *record.EpisodeID <= 0
		if err != nil || invalidSeries || invalidEpisode {
			return errors.New("invalid import identity")
		}
		if imports[record.Hash] == nil {
			imports[record.Hash] = make(map[sonarrImportRef]struct{})
		}
		ref := sonarrImportRef{seriesID: *record.SeriesID, episodeID: *record.EpisodeID, fileID: fileID}
		imports[record.Hash][ref] = struct{}{}
		series[ref.seriesID] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	current := make(map[int]sonarrImportRef)
	seriesIDs := make([]int, 0, len(series))
	for id := range series {
		seriesIDs = append(seriesIDs, id)
	}
	sort.Ints(seriesIDs)
	for _, seriesID := range seriesIDs {
		body, err := reader.get(ctx, "episode", url.Values{"seriesId": {strconv.Itoa(seriesID)}})
		if err != nil {
			return nil, err
		}
		var episodes []struct {
			ID       *int `json:"id"`
			SeriesID *int `json:"seriesId"`
			FileID   *int `json:"episodeFileId"`
		}
		if json.Unmarshal(body, &episodes) != nil || episodes == nil {
			return nil, errors.New("sonarr episode: invalid response JSON")
		}
		for _, episode := range episodes {
			if episode.ID == nil || episode.SeriesID == nil || episode.FileID == nil {
				return nil, errors.New("sonarr episode: missing or null identity")
			}
			_, duplicate := current[*episode.ID]
			invalidIdentity := *episode.ID <= 0 || *episode.FileID < 0 || *episode.SeriesID != seriesID
			if duplicate || invalidIdentity {
				return nil, errors.New("sonarr episode: invalid or duplicate identity")
			}
			current[*episode.ID] = sonarrImportRef{seriesID: seriesID, episodeID: *episode.ID, fileID: *episode.FileID}
		}
	}
	for hash, assessment := range assessments {
		refs := imports[hash]
		if len(refs) == 0 {
			continue
		}
		*assessment = assessSonarrImports(refs, current)
	}
	return assessments, nil
}

func assessSonarrImports(refs map[sonarrImportRef]struct{}, current map[int]sonarrImportRef) SonarrImportDecision {
	byEpisode := make(map[int]sonarrImportRef)
	var ambiguous, hasCurrent, missingEpisode, missingFile bool
	for ref := range refs {
		previous, exists := byEpisode[ref.episodeID]
		if exists && previous != ref {
			ambiguous = true
		}
		byEpisode[ref.episodeID] = ref
		present, exists := current[ref.episodeID]
		switch {
		case !exists || present.seriesID != ref.seriesID:
			missingEpisode = true
		case present.fileID == 0:
			missingFile = true
		case present.fileID == ref.fileID:
			hasCurrent = true
		}
	}
	decision := SonarrImportDecision{State: "unknown", ImportedEpisodes: len(byEpisode)}
	switch {
	case ambiguous:
		decision.Reason = "ambiguous_import_history"
	case hasCurrent:
		decision.State, decision.Reason = "current", "current_episode_file"
	case missingEpisode:
		decision.Reason = "episode_missing"
	case missingFile:
		decision.Reason = "episode_file_missing"
	default:
		decision.State, decision.Reason = "replaced", "different_current_files"
	}
	return decision
}
