package session

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
)

// sessionObject indexes immediate fields once. In particular, finding an absent
// session field no longer walks a multi-megabyte messages/contents array again.
type sessionObject struct {
	gjson.Result
	fields     map[string]gjson.Result
	objects    map[string]sessionObject
	duplicates map[string]bool
}

func parseSessionObject(payload []byte) sessionObject {
	return indexSessionObject(util.ParseGJSONBytesNoCopy(payload))
}

func indexSessionObject(result gjson.Result) sessionObject {
	return indexSessionObjectDepth(result, 0)
}

func indexSessionObjectDepth(result gjson.Result, depth int) sessionObject {
	out := sessionObject{Result: result}
	if !result.IsObject() {
		return out
	}
	out.fields = make(map[string]gjson.Result)
	result.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if _, exists := out.fields[name]; exists {
			if out.duplicates == nil {
				out.duplicates = make(map[string]bool)
			}
			out.duplicates[name] = true
		} else {
			out.fields[name] = value
			if depth < 2 && value.IsObject() {
				switch name {
				case "metadata", "extra_body", "forkSource", "fork_source", "conversation":
					if out.objects == nil {
						out.objects = make(map[string]sessionObject)
					}
					out.objects[name] = indexSessionObjectDepth(value, depth+1)
				}
			}
		}
		return true
	})
	return out
}

func (o sessionObject) Get(path string) gjson.Result {
	if o.fields == nil {
		return o.Result.Get(path)
	}
	key, rest, nested := strings.Cut(path, ".")
	value := o.fields[key]
	if nested {
		// GJSON continues into later duplicate containers if an earlier one
		// does not contain the requested descendant.
		if o.duplicates[key] {
			return o.Result.Get(path)
		}
		if object, ok := o.objects[key]; ok {
			return object.Get(rest)
		}
		return value.Get(rest)
	}
	return value
}
