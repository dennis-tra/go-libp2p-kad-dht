package metrics

import (
	"encoding/json"
	"io"
	"os"

	"github.com/libp2p/go-libp2p-core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const filename = "providers.json"

//A container for the encapsulated struct.
//
//File containts a json array of provider records.
//[{ProviderRecord1},{ProviderRecord2},{ProviderRecord3}]
type ProviderRecords struct {
	EncapsulatedJSONProviderRecords []EncapsulatedJSONProviderRecord `json:"ProviderRecords"`
}

//This struct will be used to create,read and store the encapsulated data necessary for reading the
//provider records.
type EncapsulatedJSONProviderRecord struct {
	ID      string         `json:"PeerID"`
	CID     string         `json:"ContentID"`
	Address []ma.Multiaddr `json:"PeerMultiaddress"`
}

//Creates a new:
//	EncapsulatedCidProvider struct {
//		ID      string
//		CID     string
//		Address ma.Multiaddr
//	}
func NewEncapsulatedJSONCidProvider(id string, cid string, address []ma.Multiaddr) EncapsulatedJSONProviderRecord {
	return EncapsulatedJSONProviderRecord{
		ID:      id,
		CID:     cid,
		Address: address,
	}
}

//Saves the providers along with the CIDs in a json format. In an error occurs it returns the error or else
//it returns nil.
//
//Because we want to add a new provider record in the file for each new provider record
//we need to read the contents and add the new provider record to the already existing array.
//TODO better error handling
func saveProvidersToFile(contentID string, addressInfos []*peer.AddrInfo) error {
	jsonFile, err := os.Open(filename)
	defer jsonFile.Close()
	if err != nil {
		return err
	}
	//create a new instance of ProviderRecords struct which is a container for the encapsulated struct
	var records ProviderRecords

	bytes, err := io.ReadAll(jsonFile)
	if err != nil {
		return err
	}

	//read the existing data
	err = json.Unmarshal(bytes, &records.EncapsulatedJSONProviderRecords)
	if err != nil {
		return err
	}

	for _, addressInfo := range addressInfos {
		//create a new encapsulated struct
		NewEncapsulatedJSONProviderRecord := EncapsulatedJSONProviderRecord{
			ID:      addressInfo.ID.Pretty(),
			CID:     contentID,
			Address: addressInfo.Addrs,
		}
		//insert the new provider record to the slice in memory containing the provider records read
		records.EncapsulatedJSONProviderRecords = append(records.EncapsulatedJSONProviderRecords, NewEncapsulatedJSONProviderRecord)
	}
	data, err := json.MarshalIndent(records.EncapsulatedJSONProviderRecords, "", " ")
	if err != nil {
		return err
	}
	err = os.WriteFile(filename, data, 0644)
	if err != nil {
		return err
	}
	return nil
}
