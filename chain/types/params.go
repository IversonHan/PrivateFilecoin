package types

import (
	"encoding/json"
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
)

type MultiParams interface {
	IsMultiMsg() bool
}


type Receive struct {
	To   address.Address
	Value abi.TokenAmount
	Method abi.MethodNum
	Params []byte
}

type ClassicalParams struct {
	Params   []Receive   `json:"params"`
}

func (cp ClassicalParams) Serialize() (error, []byte) {
	by, err := json.Marshal(cp)
	if err != nil {
		return err, nil
	}
	return nil, by
}

func (cp *ClassicalParams) Deserialize(by []byte) error {
	return json.Unmarshal(by, cp)
}