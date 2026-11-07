package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
)

// configOptionTimeout bounds a session/set_config_option round trip.
// The server answers synchronously with the refreshed option list.
const configOptionTimeout = 15 * time.Second

// setConfigOptionParams is the ACP session/set_config_option request
// shape. The option id travels as "configId" — the server rejects
// "optionId" with -32602 Invalid params.
type setConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

// SetConfigOption sets one ACP session config option (e.g. "model")
// on the live session and returns the server's response, which
// carries the full option list with the new currentValue.
//
// The refreshed option list is also the bridge's source of truth for
// the model, so the bridge reports the model that is actually in
// effect afterwards.
func (d *driver) SetConfigOption(id, value string) (json.RawMessage, error) {
	if d.sessionID == "" {
		return nil, errors.New("bridge/acp: no session id")
	}
	ctx, cancel := context.WithTimeout(d.ctx, configOptionTimeout)
	defer cancel()
	result, err := d.rpc.request(ctx, "session/set_config_option", setConfigOptionParams{
		SessionID: d.sessionID,
		ConfigID:  id,
		Value:     value,
	})
	if err != nil {
		return nil, err
	}
	d.recordConfigOptionModel(result)
	return result, nil
}

// recordConfigOptionModel captures the model selector from a
// session/set_config_option response. A response without a model
// option leaves the recorded model unchanged.
func (d *driver) recordConfigOptionModel(result json.RawMessage) {
	model := modelFromConfigOptions(configOptionsFromResult(result))
	if model == "" {
		return
	}
	d.modelMu.Lock()
	d.model = model
	d.modelMu.Unlock()
}

// configOptionsFromResult extracts the config option list from a
// session/set_config_option response. The ACP shape is
// {"configOptions": [...]}; a bare array is accepted too.
func configOptionsFromResult(result json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(result)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '[' {
		return trimmed
	}
	var response struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if json.Unmarshal(result, &response) != nil {
		return nil
	}
	return response.ConfigOptions
}
