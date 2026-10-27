package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const maxResponseBytes = 64 << 10

// permanentRejections lists Keeper's own event-level error codes. Proxies, WAFs and
// receivers speaking another protocol version answer differently and keep data pending.
var permanentRejections = map[int]string{
	http.StatusBadRequest:            "invalid_batch",
	http.StatusConflict:              "delivery_id_conflict",
	http.StatusRequestEntityTooLarge: "batch_too_large",
	http.StatusUnprocessableEntity:   "invalid_usage_event",
}

func (q *Queue) deliver(parent context.Context, events []Event) error {
	body, err := q.MarshalBatch(events)
	if err != nil {
		return err
	}
	if len(body) > MaxBatchBytes {
		return fmt.Errorf("usage batch exceeds size limit")
	}
	status, raw, err := q.post(parent, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		code := keeperErrorCode(raw)
		permanent := code != "" && permanentRejections[status] == code
		// A proxy body limit also answers 413 without Keeper's code; smaller batches may pass it.
		if (permanent || status == http.StatusRequestEntityTooLarge) && len(events) > 1 {
			mid := len(events) / 2
			if err := q.deliver(parent, events[:mid]); err != nil {
				return err
			}
			return q.deliver(parent, events[mid:])
		}
		if permanent {
			return q.quarantine(events, fmt.Sprintf("http_%d_%s", status, code))
		}
		if code != "" {
			return fmt.Errorf("usage upload returned status %d (%s)", status, code)
		}
		return fmt.Errorf("usage upload returned status %d", status)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("invalid usage acknowledgement")
	}
	var ack Acknowledgement
	if err := json.Unmarshal(raw, &ack); err != nil || ack.SchemaVersion != SchemaVersion || len(ack.AcceptedIDs) != len(events) {
		return fmt.Errorf("incomplete usage acknowledgement")
	}
	expected := make(map[string]bool, len(events))
	for _, e := range events {
		expected[e.ID] = true
	}
	for _, id := range ack.AcceptedIDs {
		if !expected[id] {
			return fmt.Errorf("unknown or repeated acknowledgement ID")
		}
		delete(expected, id)
	}
	if len(expected) != 0 {
		return fmt.Errorf("incomplete usage acknowledgement")
	}
	return q.acknowledge(events)
}

// post reads and closes the response before the caller splits a batch into new requests.
func (q *Queue) post(parent context.Context, body []byte) (int, []byte, error) {
	// Explicitly authorized telemetry-only timeout; this is never model traffic.
	ctx, cancel := context.WithTimeout(parent, q.cfg.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+q.cfg.Token)
	resp, err := q.client.Do(req)
	if err != nil {
		// The endpoint is validated to carry no credentials; the token only travels in a header.
		return 0, nil, fmt.Errorf("usage upload transport failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read usage upload response: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// keeperErrorCode returns Keeper's error identifier, or "" for foreign or malformed bodies.
func keeperErrorCode(raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil || !identifier.MatchString(body.Error) {
		return ""
	}
	return body.Error
}
