// Copyright (c) 2013-2015 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package mutilchain

import (
	"fmt"
	"strings"
)

const (
	TYPEDCR  = "dcr"
	TYPELTC  = "ltc"
	TYPEBTC  = "btc"
	TYPEDERO = "dero"
	TESTNET  = "testnet"
	MAINNET  = "mainnet"
)

func IsEmpty(x interface{}) bool {
	switch value := x.(type) {
	case string:
		return value == ""
	case int32:
		return value == 0
	case int:
		return value == 0
	case uint32:
		return value == 0
	case uint64:
		return value == 0
	case int64:
		return value == 0
	case float64:
		return value == 0
	case bool:
		return false
	default:
		return true
	}
}

func IsDisabledChain(disabledList string, chainType string) bool {
	if IsEmpty(disabledList) {
		return false
	}
	disabledArr := strings.Split(disabledList, ",")
	for _, disabledItem := range disabledArr {
		if IsEmpty(disabledItem) {
			continue
		}
		if strings.TrimSpace(disabledItem) == chainType {
			fmt.Println("return exist: ", chainType)
			return true
		}
	}
	return false
}
