package rules

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// errOmit is returned by convert when the expression asked for the field to be
// left out: `optional.none()` or `null`.
var errOmit = errors.New("omit")

var stringType = reflect.TypeOf("")

// convert turns a CEL result into a plain JSON-compatible Go value.
//
// It is deliberately not implemented with structpb, because that converts every
// number to float64 and would therefore turn int64 status fields into floats.
// `optional.of(x)` is unwrapped to x, `optional.none()` and `null` report
// errOmit.
func convert(v ref.Val) (any, error) {
	if v == nil {
		return nil, errOmit
	}
	if err, ok := v.(*types.Err); ok {
		return nil, err
	}
	if opt, ok := v.(*types.Optional); ok {
		if !opt.HasValue() {
			return nil, errOmit
		}
		return convert(opt.GetValue())
	}

	switch val := v.(type) {
	case types.Bool:
		return bool(val), nil
	case types.Bytes:
		return base64.StdEncoding.EncodeToString([]byte(val)), nil
	case types.Double:
		return float64(val), nil
	case types.Int:
		return int64(val), nil
	case types.String:
		return string(val), nil
	case types.Uint:
		if uint64(val) > 1<<63-1 {
			return nil, fmt.Errorf("unsigned value %d overflows int64", uint64(val))
		}
		return int64(val), nil
	case types.Duration:
		return val.String(), nil
	case types.Timestamp:
		return val.UTC().Format(time.RFC3339), nil
	case types.Null:
		return nil, errOmit
	}

	switch val := v.(type) {
	case traits.Lister:
		return convertList(val)
	case traits.Mapper:
		return convertMap(val)
	}

	// Anything else (k8s IP/CIDR/Quantity/URL/Semver wrappers, …) is rendered
	// through its string representation, which is what the API types expect.
	if s, err := v.ConvertToNative(stringType); err == nil {
		if str, ok := s.(string); ok {
			return str, nil
		}
	}
	return nil, fmt.Errorf("cannot convert a value of CEL type %s to a status value", v.Type())
}

func convertList(l traits.Lister) (any, error) {
	size, ok := l.Size().(types.Int)
	if !ok {
		return nil, fmt.Errorf("cannot determine the size of a list value")
	}
	out := make([]any, 0, int(size))
	for i := int64(0); i < int64(size); i++ {
		item, err := convert(l.Get(types.Int(i)))
		if err != nil {
			// A null inside a list stays a null: only top-level status fields
			// and map entries can be omitted.
			if errors.Is(err, errOmit) {
				out = append(out, nil)
				continue
			}
			return nil, fmt.Errorf("list element %d: %w", i, err)
		}
		out = append(out, item)
	}
	return out, nil
}

func convertMap(m traits.Mapper) (any, error) {
	out := map[string]any{}
	it := m.Iterator()
	for it.HasNext() == types.True {
		k := it.Next()
		key, err := convert(k)
		if err != nil {
			return nil, fmt.Errorf("map key: %w", err)
		}
		name, ok := key.(string)
		if !ok {
			name = fmt.Sprintf("%v", key)
		}
		value, err := convert(m.Get(k))
		if err != nil {
			if errors.Is(err, errOmit) {
				continue
			}
			return nil, fmt.Errorf("map key %q: %w", name, err)
		}
		out[name] = value
	}
	return out, nil
}
