package cleaner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type transmissionSession struct {
	Version          *string `json:"version"`
	RPCVersionSemver *string `json:"rpc_version_semver"`
}

var errRPCRejected = errors.New("rpc rejected before execution")

type transmissionTracker struct {
	Announce *string `json:"announce"`
}

type transmissionTorrent struct {
	HashString              *string                `json:"hash_string"`
	Name                    *string                `json:"name"`
	DownloadDir             *string                `json:"download_dir"`
	UploadRatio             *float64               `json:"upload_ratio"`
	SecondsSeeding          json.RawMessage        `json:"seconds_seeding"`
	Status                  *int                   `json:"status"`
	IsFinished              *bool                  `json:"is_finished"`
	LeftUntilDone           json.RawMessage        `json:"left_until_done"`
	MetadataPercentComplete *float64               `json:"metadata_percent_complete"`
	Trackers                *[]transmissionTracker `json:"trackers"`
	Labels                  *[]string              `json:"labels"`
}

type transmissionRPC struct {
	endpoint      string
	credentials   Credentials
	sessionID     string
	handshakeUsed bool
	requestID     int
	allowWrites   bool
}

func ReadTransmission(ctx context.Context, c Config) ([]Torrent, error) {
	rpc, err := newTransmissionRPC(ctx, c)
	if err != nil {
		return nil, err
	}
	return rpc.readTorrents(ctx, nil)
}

func newTransmissionRPC(ctx context.Context, c Config) (*transmissionRPC, error) {
	if c.Credentials.TransmissionUser == "" || c.Credentials.TransmissionPassword == "" || strings.ContainsAny(c.Credentials.TransmissionUser+c.Credentials.TransmissionPassword, "\r\n") {
		return nil, errors.New("transmission: credentials are missing or invalid")
	}
	u, err := parseEndpoint(c.Transmission.URL, true)
	if err != nil {
		return nil, errors.New("transmission: invalid endpoint")
	}
	rpc := &transmissionRPC{endpoint: u.String(), credentials: c.Credentials, allowWrites: !c.IsDryRun()}
	var session transmissionSession
	if err := rpc.call(ctx, "session_get", struct {
		Fields []string `json:"fields"`
	}{[]string{"version", "rpc_version_semver"}}, &session); err != nil {
		return nil, err
	}

	if session.Version == nil || session.RPCVersionSemver == nil || !supportedTransmissionVersion(*session.Version, *session.RPCVersionSemver) {
		return nil, errors.New("transmission: unsupported or incomplete version response")
	}

	return rpc, nil
}

func (rpc *transmissionRPC) readTorrents(ctx context.Context, ids []string) ([]Torrent, error) {
	torrentFields := []string{
		"hash_string", "name", "download_dir", "upload_ratio", "seconds_seeding",
		"status", "is_finished", "left_until_done", "metadata_percent_complete", "trackers", "labels",
	}
	var torrentResponse struct {
		Torrents *[]transmissionTorrent `json:"torrents"`
	}
	if err := rpc.call(ctx, "torrent_get", struct {
		Fields []string `json:"fields"`
		IDs    []string `json:"ids,omitempty"`
	}{torrentFields, ids}, &torrentResponse); err != nil {
		return nil, err
	}

	if torrentResponse.Torrents == nil {
		return nil, errors.New("transmission: missing or null torrents list")
	}

	torrents := make([]Torrent, 0, len(*torrentResponse.Torrents))
	seen := make(map[string]struct{}, len(*torrentResponse.Torrents))
	for _, raw := range *torrentResponse.Torrents {
		item, err := raw.domainTorrent()
		if err != nil {
			return nil, errors.New("transmission: invalid torrent data")
		}
		if _, exists := seen[item.Hash]; exists {
			return nil, errors.New("transmission: duplicate torrent hash")
		}
		seen[item.Hash] = struct{}{}
		torrents = append(torrents, item)
	}
	sort.Slice(torrents, func(i, j int) bool { return torrents[i].Hash < torrents[j].Hash })
	return torrents, nil
}

// call reuses the encoded request only for the bounded 409 session handshake.
func (rpc *transmissionRPC) call(ctx context.Context, method string, params any, result any) error {
	if !rpc.allowWrites && method != "session_get" && method != "torrent_get" {
		return errors.New("transmission: mutating RPC is forbidden in dry-run")
	}
	rpc.requestID++
	id := rpc.requestID
	request := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
		ID      int    `json:"id"`
	}{"2.0", method, params, id}
	body, err := json.Marshal(request)
	if err != nil {
		return errors.New("transmission: failed to encode " + method)
	}
	for attempt := 0; attempt < 2; attempt++ {
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		if rpc.sessionID != "" {
			headers.Set("X-Transmission-Session-Id", rpc.sessionID)
		}
		response, err := doAPIRequest(
			ctx,
			http.MethodPost,
			rpc.endpoint,
			"transmission "+method,
			headers,
			body,
			rpc.credentials.TransmissionUser,
			rpc.credentials.TransmissionPassword,
		)
		if err != nil {
			return err
		}
		if response.status == http.StatusConflict {
			if rpc.handshakeUsed {
				return errors.New("transmission: repeated session handshake required")
			}
			sessionID := strings.TrimSpace(response.header.Get("X-Transmission-Session-Id"))
			if sessionID == "" || strings.ContainsAny(sessionID, "\r\n") {
				return errors.New("transmission: 409 response omitted session id")
			}
			rpc.sessionID = sessionID
			rpc.handshakeUsed = true
			continue
		}
		if response.status != http.StatusOK {
			return fmt.Errorf("transmission %s: HTTP status %d", method, response.status)
		}
		return decodeTransmissionRPC(response.body, id, result, method)
	}
	return errors.New("transmission: request retry limit exceeded")
}

func decodeTransmissionRPC(body []byte, expectedID int, result any, method string) error {
	var envelope struct {
		JSONRPC *string         `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.JSONRPC == nil || *envelope.JSONRPC != "2.0" {
		return errors.New("transmission " + method + ": invalid JSON-RPC response")
	}
	var responseID int
	if len(envelope.ID) == 0 || json.Unmarshal(envelope.ID, &responseID) != nil || responseID != expectedID {
		return errors.New("transmission " + method + ": response id mismatch")
	}
	if len(envelope.Error) != 0 && !bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) {
		// Only standard pre-dispatch errors prove that remove did not execute.
		// Internal/application errors and conflicting envelopes remain uncertain.
		if method == "torrent_remove" && len(envelope.Result) == 0 {
			var rejection struct {
				Code    *int    `json:"code"`
				Message *string `json:"message"`
			}
			validError := json.Unmarshal(envelope.Error, &rejection) == nil
			hasFields := rejection.Code != nil && rejection.Message != nil
			if validError && hasFields {
				switch *rejection.Code {
				case -32600, -32601, -32602:
					return fmt.Errorf("transmission %s: %w", method, errRPCRejected)
				}
			}
		}
		return errors.New("transmission " + method + ": RPC returned an error")
	}
	if len(envelope.Result) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Result), []byte("null")) {
		return errors.New("transmission " + method + ": missing or null result")
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return errors.New("transmission " + method + ": invalid result")
	}
	return nil
}

func supportedTransmissionVersion(version, rpcVersion string) bool {
	appVersion := strings.Fields(version)
	if len(appVersion) == 0 || !versionIs(appVersion[0], "4", "1") || !versionIs(rpcVersion, "6", "0") {
		return false
	}
	return true
}

func versionIs(version, major, minor string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 || parts[0] != major || parts[1] != minor || parts[2] == "" {
		return false
	}
	for _, c := range parts[2] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (raw transmissionTorrent) domainTorrent() (Torrent, error) {
	if raw.HashString == nil || raw.Name == nil || raw.DownloadDir == nil || raw.UploadRatio == nil || raw.Status == nil || raw.IsFinished == nil || raw.MetadataPercentComplete == nil || raw.Trackers == nil || raw.Labels == nil {
		return Torrent{}, errors.New("required field missing or null")
	}
	secondsSeeding, err := jsonInteger(raw.SecondsSeeding)
	if err != nil {
		return Torrent{}, err
	}
	leftUntilDone, err := jsonInteger(raw.LeftUntilDone)
	if err != nil {
		return Torrent{}, err
	}
	hash, err := canonicalHash(*raw.HashString)
	if err != nil {
		return Torrent{}, err
	}
	trackers := make([]string, 0, len(*raw.Trackers))
	for _, tracker := range *raw.Trackers {
		if tracker.Announce == nil {
			return Torrent{}, errors.New("tracker announce missing or null")
		}
		trackers = append(trackers, *tracker.Announce)
	}
	for _, label := range *raw.Labels {
		if label == "" || strings.TrimSpace(label) != label || strings.Contains(label, ",") {
			return Torrent{}, errors.New("invalid torrent label")
		}
	}
	item := Torrent{
		Hash:             hash,
		Name:             *raw.Name,
		DownloadDir:      *raw.DownloadDir,
		Ratio:            *raw.UploadRatio,
		SecondsSeeding:   secondsSeeding,
		Status:           *raw.Status,
		IsFinished:       *raw.IsFinished,
		LeftUntilDone:    leftUntilDone,
		MetadataComplete: *raw.MetadataPercentComplete,
		Trackers:         trackers,
		Labels:           append([]string{}, (*raw.Labels)...),
	}
	if err := item.validate(); err != nil {
		return Torrent{}, err
	}
	return item, nil
}

// Counters may use integral decimal/exponent notation. Parse exactly without
// float64 rounding, and bound the input before math/big can allocate for it.
func jsonInteger(raw json.RawMessage) (int64, error) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || len(value) > 64 || bytes.Equal(value, []byte("null")) {
		return 0, errors.New("integer field missing or null")
	}
	if !json.Valid(value) {
		return 0, errors.New("integer field is not a JSON number")
	}
	spelling := string(value)
	if spelling[0] != '-' && (spelling[0] < '0' || spelling[0] > '9') {
		return 0, errors.New("integer field is not a JSON number")
	}
	if at := strings.IndexAny(spelling, "eE"); at >= 0 {
		exponent, err := strconv.ParseInt(spelling[at+1:], 10, 64)
		if err != nil || exponent < -64 || exponent > 64 {
			return 0, errors.New("integer field exponent is out of range")
		}
	}
	rational, ok := new(big.Rat).SetString(spelling)
	if !ok || !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, errors.New("integer field is out of range")
	}
	return rational.Num().Int64(), nil
}

func (rpc *transmissionRPC) torrent(ctx context.Context, hash string) (*Torrent, error) {
	torrents, err := rpc.readTorrents(ctx, []string{hash})
	if err != nil {
		return nil, err
	}
	if len(torrents) == 0 {
		return nil, nil
	}
	if len(torrents) != 1 || torrents[0].Hash != hash {
		return nil, errors.New("transmission: unexpected torrent in targeted read")
	}
	return &torrents[0], nil
}

func (rpc *transmissionRPC) setLabels(ctx context.Context, hash string, labels []string) error {
	canonical, err := canonicalHash(hash)
	if err != nil || canonical != hash || labels == nil {
		return errors.New("transmission: invalid label update")
	}
	var result struct{}
	return rpc.call(ctx, "torrent_set", struct {
		IDs    []string `json:"ids"`
		Labels []string `json:"labels"`
	}{[]string{hash}, labels}, &result)
}

func (rpc *transmissionRPC) remove(ctx context.Context, hash string) error {
	canonical, err := canonicalHash(hash)
	if err != nil || canonical != hash {
		return errors.New("transmission: invalid remove hash")
	}
	var result struct{}
	return rpc.call(ctx, "torrent_remove", struct {
		IDs             []string `json:"ids"`
		DeleteLocalData bool     `json:"delete_local_data"`
	}{IDs: []string{hash}, DeleteLocalData: true}, &result)
}
