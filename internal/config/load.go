package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
)

// maxFileSize bounds how much Load reads. A real config is a few kilobytes.
const maxFileSize = 1 << 20

// Load reads, decodes and validates the config file at path. Along with the
// config it returns warnings to show the user, such as a file that other
// users can read. When the file doesn't exist, the error satisfies
// errors.Is(err, fs.ErrNotExist).
func Load(path string) (*Config, []string, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, warnings, err := Parse(data)
	if err != nil {
		return nil, nil, fmt.Errorf("config %s: %w", path, err)
	}
	if w, err := checkPrivate(path); err != nil {
		warnings = append([]string{fmt.Sprintf("can't check who may read %s: %v", path, err)}, warnings...)
	} else if w != "" {
		warnings = append([]string{w}, warnings...)
	}
	return cfg, warnings, nil
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("config %s is larger than %d bytes", path, maxFileSize)
	}
	return data, nil
}

// Parse decodes and validates the content of a config file.
func Parse(data []byte) (*Config, []string, error) {
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		// Windows PowerShell 5.1 writes UTF-16 with ">" and Out-File.
		return nil, nil, errors.New("the file is UTF-16 encoded; save it as UTF-8")
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // byte order mark from Notepad
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil, errors.New("the file is empty")
	}
	if err := checkKeys(data, reflect.TypeFor[Config]()); err != nil {
		return nil, nil, describeJSONError(data, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, describeJSONError(data, err)
	}
	cfg.applyDefaults()
	warnings, err := cfg.Validate()
	if err != nil {
		return nil, nil, err
	}
	return &cfg, warnings, nil
}

// describeJSONError turns a decoding error into a message with a line and
// column instead of a byte offset.
func describeJSONError(data []byte, err error) error {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		line, col := position(data, syntax.Offset)
		return fmt.Errorf("line %d, column %d: %s", line, col, syntax.Error())
	case errors.As(err, &typ):
		line, col := position(data, typ.Offset)
		field := typ.Field
		if field == "" {
			field = "the top-level value"
		}
		return fmt.Errorf("line %d, column %d: %s must be %s, not a JSON %s", line, col, field, kindName(typ.Type), typ.Value)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return errors.New("the JSON ends too early (is a closing bracket missing?)")
	}
	return err
}

func position(data []byte, offset int64) (line, col int) {
	offset = min(max(offset, 0), int64(len(data)))
	before := data[:offset]
	line = 1 + bytes.Count(before, []byte{'\n'})
	col = len(before) - bytes.LastIndexByte(before, '\n')
	return line, col
}

func kindName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int64, reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Slice:
		return "a list"
	case reflect.Map, reflect.Struct, reflect.Pointer:
		return "an object"
	}
	return t.String()
}
