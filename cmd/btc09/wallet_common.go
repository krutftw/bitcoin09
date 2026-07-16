package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/krutftw/bitcoin09/core"
)

func parseCoinAmount(text string, allowZero bool) (int64, error) {
	if text == "" || len(text) > 32 {
		return 0, errors.New("invalid decimal length")
	}
	for _, character := range []byte(text) {
		if (character < '0' || character > '9') && character != '.' {
			return 0, errors.New("amount must be plain ASCII decimal")
		}
	}
	parts := strings.Split(text, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") || (len(parts) == 2 && len(parts[1]) > 8) {
		return 0, errors.New("amount must have one to eight fractional digits")
	}
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || whole > 21_000_000 {
		return 0, errors.New("amount exceeds maximum supply")
	}
	var fraction uint64
	if len(parts) == 2 {
		fractionText := parts[1] + strings.Repeat("0", 8-len(parts[1]))
		fraction, err = strconv.ParseUint(fractionText, 10, 64)
		if err != nil {
			return 0, errors.New("invalid fractional amount")
		}
	}
	if whole == 21_000_000 && fraction != 0 {
		return 0, errors.New("amount exceeds maximum supply")
	}
	units := whole*uint64(core.UnitsPerCoin) + fraction
	if units > uint64(core.MaxMoneyUnits) || (!allowZero && units == 0) {
		return 0, errors.New("amount out of range")
	}
	return int64(units), nil
}

func formatOutpoint(outpoint core.OutPoint) string {
	return fmt.Sprintf("%x:%d", outpoint.TxID, outpoint.Idx)
}
