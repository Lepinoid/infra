package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

var ErrSchema = errors.New("invalid persistent schema")

func Decode[T any](r io.Reader) (T, error) {
	var result T
	data, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return result, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := unique(d); err != nil {
		return result, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return result, ErrSchema
	}
	if err := required(data, reflect.TypeFor[T]()); err != nil {
		return result, err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&result); err != nil {
		return result, err
	}
	var version struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return result, err
	}
	if version.SchemaVersion != 1 {
		return result, ErrSchema
	}
	return result, nil
}

func unique(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrSchema
			}
			seen[name] = true
			if err := unique(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := unique(d); err != nil {
				return err
			}
		}
	default:
		return ErrSchema
	}
	_, err = d.Token()
	return err
}

func required(data []byte, t reflect.Type) error {
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return nil
		}
		return required(data, t.Elem())
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return ErrSchema
	}
	if t == reflect.TypeFor[time.Time]() {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		if !strings.HasSuffix(text, "Z") {
			return ErrSchema
		}
		_, err := time.Parse(time.RFC3339Nano, text)
		return err
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				if err := required(data, f.Type); err != nil {
					return err
				}
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			raw, ok := fields[name]
			if !ok {
				return fmt.Errorf("%w: missing %s", ErrSchema, name)
			}
			if err := required(raw, f.Type); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	case reflect.Slice:
		var entries []json.RawMessage
		if err := json.Unmarshal(data, &entries); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := required(entry, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
