package validator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type ValidatorUptime struct {
	ValidationID  string
	UptimeSeconds uint64
  NodeID        string
	IsActive      bool
}

type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	Result  *struct {
		Validators []struct {
			ValidationID  string `json:"validationID"`
			UptimeSeconds uint64 `json:"uptimeSeconds"`
      NodeID        string `json:"nodeID"`
			IsActive      bool   `json:"isActive"`
		} `json:"validators"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func FetchUptimes(apiBaseURL string) ([]ValidatorUptime, error) {
	reqBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"validators.getCurrentValidators","params":{}}`)
	url := apiBaseURL + "/validators"
	resp, err := http.Post(url, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to call Avalanche validators API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d from Avalanche API", resp.StatusCode)
	}

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("failed to parse Avalanche API response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("Avalanche API error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if rpcResp.Result == nil {
		return nil, errors.New("invalid response: missing result")
	}

	validators := make([]ValidatorUptime, 0, len(rpcResp.Result.Validators))
	for _, v := range rpcResp.Result.Validators {
		validators = append(validators, ValidatorUptime{
			ValidationID:  v.ValidationID,
			UptimeSeconds: v.UptimeSeconds,
      NodeID:        v.NodeID,
			IsActive:      v.IsActive,
		})
	}
	return validators, nil
}
