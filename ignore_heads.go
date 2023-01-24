package dht

import (
	"bytes"
	_ "embed"
	"encoding/csv"
)

//go:embed peers.csv
var peersFile []byte

func init() {
	r := csv.NewReader(bytes.NewReader(peersFile))
	records, err := r.ReadAll()
	if err != nil {
		panic(err)
	}

	for _, record := range records {
		ignoredPeers[record[0]] = true
	}
}

var ignoredPeers = map[string]bool{}
