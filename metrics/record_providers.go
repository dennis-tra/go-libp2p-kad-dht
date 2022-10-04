package metrics

import (
	"encoding/json"
	"io"
	"os"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/prometheus/common/log"
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
	ID        string   `json:"PeerID"`
	CID       string   `json:"ContentID"`
	Addresses []string `json:"PeerMultiaddress"`
}

//Creates a new:
//	EncapsulatedCidProvider struct {
//		ID      string
//		CID     string
//		Address ma.Multiaddr
//	}
func NewEncapsulatedJSONCidProvider(id string, cid string, addresses []string) EncapsulatedJSONProviderRecord {
	return EncapsulatedJSONProviderRecord{
		ID:        id,
		CID:       cid,
		Addresses: addresses,
	}
}

//Saves the providers along with the CIDs in a json format. In an error occurs it returns the error or else
//it returns nil.
//
//Because we want to add a new provider record in the file for each new provider record
//we need to read the contents and add the new provider record to the already existing array.
func saveProvidersToFile(contentID string, addressInfos []*peer.AddrInfo) error {
	jsonFile, err := os.Open(filename)
	defer func(jsonFile *os.File) {
		err := jsonFile.Close()
		if err != nil {
			log.Errorf("error %s while closing down providers file", err)
		}
	}(jsonFile)
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

		addressesString := make([]string, 0)
		for _, address := range addressInfo.Addrs {
			addressesString = append(addressesString, address.String())
		}
		//create a new encapsulated struct
		NewEncapsulatedJSONProviderRecord := EncapsulatedJSONProviderRecord{
			ID:        addressInfo.ID.Pretty(),
			CID:       contentID,
			Addresses: addressesString,
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
