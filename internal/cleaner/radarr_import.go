package cleaner

import (
	"context"
	"encoding/json"
	"errors"
)

// RadarrImportDecision is an informational snapshot, independent of cleanup policy.
type RadarrImportDecision struct {
	State   string `json:"state"`
	Reason  string `json:"reason"`
	MovieID int    `json:"movie_id,omitempty"`
	FileID  int    `json:"file_id,omitempty"`
}

type radarrImportRef struct {
	movieID int
	fileID  int
}

func readRadarrImports(ctx context.Context, c Config, torrents []Torrent) (map[string]*RadarrImportDecision, error) {
	assessments := make(map[string]*RadarrImportDecision)
	for hash := range importedHashes(torrents, c.Radarr.ImportedLabel) {
		assessments[hash] = &RadarrImportDecision{State: "unknown", Reason: "no_import_history"}
	}
	if len(assessments) == 0 {
		return assessments, nil
	}
	reader := arrReader{endpoint: c.Radarr.Endpoint, apiKey: c.Credentials.RadarrKey, service: "radarr"}
	imports := make(map[string]map[radarrImportRef]struct{})
	err := readImportHistory(ctx, reader, func(record importHistoryRecord) error {
		hash := record.Hash
		if assessments[hash] == nil {
			return nil
		}
		fileID, err := record.fileID()
		if err != nil || record.MovieID == nil || *record.MovieID <= 0 {
			return errors.New("invalid import identity")
		}
		if imports[hash] == nil {
			imports[hash] = make(map[radarrImportRef]struct{})
		}
		imports[hash][radarrImportRef{movieID: *record.MovieID, fileID: fileID}] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	body, err := reader.get(ctx, "movie", nil)
	if err != nil {
		return nil, err
	}
	var movies []struct {
		ID     *int `json:"id"`
		FileID *int `json:"movieFileId"`
	}
	if json.Unmarshal(body, &movies) != nil || movies == nil {
		return nil, errors.New("radarr movie: invalid response JSON")
	}
	current := make(map[int]int)
	for _, movie := range movies {
		if movie.ID == nil || movie.FileID == nil {
			return nil, errors.New("radarr movie: missing or null identity")
		}
		_, duplicate := current[*movie.ID]
		if duplicate || *movie.ID <= 0 || *movie.FileID < 0 {
			return nil, errors.New("radarr movie: invalid or duplicate identity")
		}
		current[*movie.ID] = *movie.FileID
	}
	for hash, assessment := range assessments {
		refs := imports[hash]
		if len(refs) == 0 {
			continue
		}
		if len(refs) > 1 {
			assessment.Reason = "ambiguous_import_history"
			continue
		}
		for ref := range refs {
			assessment.MovieID, assessment.FileID = ref.movieID, ref.fileID
			fileID, exists := current[ref.movieID]
			switch {
			case !exists:
				assessment.Reason = "movie_missing"
			case fileID == 0:
				assessment.Reason = "movie_file_missing"
			case fileID == ref.fileID:
				assessment.State, assessment.Reason = "current", "current_file"
			default:
				assessment.State, assessment.Reason = "replaced", "different_current_file"
			}
		}
	}
	return assessments, nil
}
