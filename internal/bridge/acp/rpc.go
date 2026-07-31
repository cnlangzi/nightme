package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

const jsonRPCVersion = "2.0"

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

type rpcClient struct {
	writer io.Writer

	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse
	nextID    atomic.Int64
}

func newRPCClient(writer io.Writer) *rpcClient {
	return &rpcClient{
		writer:  writer,
		pending: make(map[string]chan rpcResponse),
	}
}

func (c *rpcClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}

	paramsJSON, err := marshalParams(params)
	if err != nil {
		return nil, err
	}

	key := string(idJSON)
	responseCh := make(chan rpcResponse, 1)
	c.pendingMu.Lock()
	c.pending[key] = responseCh
	c.pendingMu.Unlock()

	msg := rpcMessage{
		JSONRPC: jsonRPCVersion,
		ID:      idJSON,
		Method:  method,
		Params:  paramsJSON,
	}
	if err := c.write(msg); err != nil {
		c.removePending(key)
		return nil, err
	}

	select {
	case response := <-responseCh:
		return response.result, response.err
	case <-ctx.Done():
		c.removePending(key)
		return nil, ctx.Err()
	}
}

func (c *rpcClient) notify(method string, params any) error {
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{
		JSONRPC: jsonRPCVersion,
		Method:  method,
		Params:  paramsJSON,
	})
}

func (c *rpcClient) requestAsync(method string, params any) error {
	id := c.nextID.Add(1)
	idJSON, err := json.Marshal(id)
	if err != nil {
		return err
	}
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{
		JSONRPC: jsonRPCVersion,
		ID:      idJSON,
		Method:  method,
		Params:  paramsJSON,
	})
}

func (c *rpcClient) respond(id json.RawMessage, result any, responseErr *rpcError) error {
	resultJSON, err := marshalParams(result)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Result:  resultJSON,
		Error:   responseErr,
	})
}

func (c *rpcClient) handleResponse(msg rpcMessage) bool {
	if len(msg.ID) == 0 || msg.Method != "" {
		return false
	}

	key := string(msg.ID)
	c.pendingMu.Lock()
	responseCh, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.pendingMu.Unlock()
	if !ok {
		return false
	}

	if msg.Error != nil {
		responseCh <- rpcResponse{err: msg.Error}
	} else {
		responseCh <- rpcResponse{result: msg.Result}
	}
	return true
}

func (c *rpcClient) failPending(err error) {
	if err == nil {
		err = io.EOF
	}
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan rpcResponse)
	c.pendingMu.Unlock()
	for _, responseCh := range pending {
		responseCh <- rpcResponse{err: err}
	}
}

func (c *rpcClient) removePending(key string) {
	c.pendingMu.Lock()
	delete(c.pending, key)
	c.pendingMu.Unlock()
}

func (c *rpcClient) write(msg rpcMessage) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.writer.Write(payload)
	return err
}

func decodeRPCMessage(line []byte) (rpcMessage, error) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return rpcMessage{}, err
	}
	if msg.JSONRPC != jsonRPCVersion {
		return rpcMessage{}, errors.New("invalid json-rpc version")
	}
	if msg.Method == "" && len(msg.ID) == 0 {
		return rpcMessage{}, errors.New("invalid json-rpc message")
	}
	return msg, nil
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(params)
}
