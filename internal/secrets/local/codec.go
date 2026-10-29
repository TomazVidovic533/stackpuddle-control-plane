package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func encodePlain(p plainStore) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("local: encode store: %w", err)
	}
	return data, nil
}

// decodePlain decodes the decrypted JSON strictly: unknown fields and trailing data are refused.
func decodePlain(data []byte, out *plainStore) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("decode store: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("decode store: data after the document")
	}
	if out.StoreVersion != storeVersion {
		return fmt.Errorf("store version %d is not %d", out.StoreVersion, storeVersion)
	}
	return nil
}
