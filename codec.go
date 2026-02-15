package eventbus

import "encoding/json"

// Codec defines how user event payloads are serialized and deserialized.
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

// JSONCodec is the default Codec using encoding/json.
type JSONCodec struct{}

func (JSONCodec) Marshal(v any) ([]byte, error)          { return json.Marshal(v) }
func (JSONCodec) Unmarshal(data []byte, v any) error      { return json.Unmarshal(data, v) }
