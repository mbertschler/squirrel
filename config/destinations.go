package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
)

// destSchema declares the parameter schema for one destination type. The
// schema drives validation (required/optional fields, secret handling) and
// the rclone.conf rendering — every param key here that resolves to a
// non-empty value is written verbatim to rclone.conf, except for "root"
// which is squirrel's own concept (we use it to compose the destination URI
// passed to rclone, not as an rclone backend param).
type destSchema struct {
	// rcloneType is the value written as `type = ...` in rclone.conf for
	// this destination. Empty means "no rclone remote" (used by the local
	// backend, which is addressed by absolute path).
	rcloneType string
	// requiredString fields must be present as plain strings and non-empty.
	requiredString []string
	// optionalString fields are passed through if set; absent is fine.
	optionalString []string
	// secretFields accept either a string literal or an inline table
	// { env = "VAR" }. The resolved literal is written to rclone.conf.
	secretFields []string
}

// destSchemas registers every supported destination type. Adding a new type
// here is the only place that needs to change to support it end-to-end;
// validateParams and renderParams loop over the schema.
var destSchemas = map[string]destSchema{
	"local": {
		rcloneType: "",
		// root is validated separately (every type needs one) and is not
		// an rclone backend param for the local case.
	},
	"sftp": {
		rcloneType:     "sftp",
		requiredString: []string{"host", "user"},
		optionalString: []string{"port", "key_file"},
		secretFields:   []string{"password"},
	},
	"s3": {
		rcloneType:     "s3",
		requiredString: []string{"provider", "bucket"},
		optionalString: []string{"region", "endpoint"},
		secretFields:   []string{"access_key_id", "secret_access_key"},
	},
	"b2": {
		rcloneType:     "b2",
		requiredString: []string{"bucket"},
		secretFields:   []string{"account_id", "application_key"},
	},
	"gcs": {
		rcloneType:     "gcs",
		requiredString: []string{"bucket"},
		optionalString: []string{"service_account_file"},
		secretFields:   []string{"service_account_credentials"},
	},
}

// SupportedTypes returns the sorted list of destination types squirrel
// knows how to render into rclone.conf. Used by error messages so users
// see what they could have typed.
func SupportedTypes() []string {
	out := make([]string, 0, len(destSchemas))
	for t := range destSchemas {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func resolveDestination(name string, raw map[string]any) (*Destination, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid destination name (must match %s)", nameRE)
	}
	typ, ok := raw["type"].(string)
	if !ok || typ == "" {
		return nil, errors.New("type is required")
	}
	schema, ok := destSchemas[typ]
	if !ok {
		return nil, fmt.Errorf("unsupported destination type %q (supported: %v)", typ, SupportedTypes())
	}
	rootAny, ok := raw["root"]
	if !ok {
		return nil, errors.New("root is required")
	}
	root, ok := rootAny.(string)
	if !ok || root == "" {
		return nil, errors.New("root must be a non-empty string")
	}
	params, err := validateAndResolveParams(schema, raw)
	if err != nil {
		return nil, err
	}
	return &Destination{Name: name, Type: typ, Root: root, Params: params}, nil
}

// validateAndResolveParams walks the schema, pulling each declared field
// out of raw and (for secrets) resolving { env = "..." } references. After
// the walk, any keys still in raw beyond {type, root, schema-known} are
// unknown fields and surface as an error — strictness keeps typos from
// silently disabling a field at rclone time.
func validateAndResolveParams(schema destSchema, raw map[string]any) (map[string]string, error) {
	out := make(map[string]string)
	seen := map[string]bool{"type": true, "root": true}
	for _, key := range schema.requiredString {
		v, err := requireString(raw, key)
		if err != nil {
			return nil, err
		}
		out[key] = v
		seen[key] = true
	}
	for _, key := range schema.optionalString {
		v, err := optionalString(raw, key)
		if err != nil {
			return nil, err
		}
		if v != "" {
			out[key] = v
		}
		seen[key] = true
	}
	for _, key := range schema.secretFields {
		v, err := resolveSecret(raw, key)
		if err != nil {
			return nil, err
		}
		if v != "" {
			out[key] = v
		}
		seen[key] = true
	}
	for k := range raw {
		if !seen[k] {
			return nil, fmt.Errorf("unknown field %q", k)
		}
	}
	return out, nil
}

func requireString(raw map[string]any, key string) (string, error) {
	v, ok := raw[key]
	if !ok {
		return "", fmt.Errorf("%s is required", key)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}
	return s, nil
}

func optionalString(raw map[string]any, key string) (string, error) {
	v, ok := raw[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

// resolveSecret accepts either a plain string or an inline table of the
// form { env = "VAR_NAME" } and returns the resolved literal. An env
// reference whose variable is unset is a load-time error — we'd rather fail
// before invoking rclone than have rclone fail with a more confusing
// message about a missing credential.
func resolveSecret(raw map[string]any, key string) (string, error) {
	v, ok := raw[key]
	if !ok {
		return "", nil
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case map[string]any:
		env, ok := t["env"].(string)
		if !ok || env == "" {
			return "", fmt.Errorf("%s: inline table must have non-empty `env` key", key)
		}
		for k := range t {
			if k != "env" {
				return "", fmt.Errorf("%s: unknown inline key %q (only `env` is supported)", key, k)
			}
		}
		val := os.Getenv(env)
		if val == "" {
			return "", fmt.Errorf("%s: environment variable %s is not set", key, env)
		}
		return val, nil
	default:
		return "", fmt.Errorf("%s must be a string or { env = \"VAR\" }", key)
	}
}

// RcloneSection returns the rclone.conf section body for this destination,
// or the empty string for type=local (which doesn't need a named remote —
// rclone treats absolute paths as local-filesystem destinations directly).
// The returned bytes do not include a trailing newline.
func (d *Destination) RcloneSection() string {
	schema := destSchemas[d.Type]
	if schema.rcloneType == "" {
		return ""
	}
	var sb stringBuilder
	sb.printf("[%s]\n", d.Name)
	sb.printf("type = %s\n", schema.rcloneType)
	// Stable ordering: required → optional → secret, alphabetical within
	// each band. Stable output makes the rendered file diffable.
	for _, key := range sortedSubset(schema.requiredString) {
		if v, ok := d.Params[key]; ok {
			sb.printf("%s = %s\n", key, v)
		}
	}
	for _, key := range sortedSubset(schema.optionalString) {
		if v, ok := d.Params[key]; ok {
			sb.printf("%s = %s\n", key, v)
		}
	}
	for _, key := range sortedSubset(schema.secretFields) {
		if v, ok := d.Params[key]; ok {
			sb.printf("%s = %s\n", key, v)
		}
	}
	return sb.String()
}

// stringBuilder wraps strings.Builder to make printf-style appends terse
// inside RcloneSection. Errors from Builder.WriteString are documented as
// always nil so we ignore them.
type stringBuilder struct {
	b []byte
}

func (s *stringBuilder) printf(format string, args ...any) {
	s.b = append(s.b, fmt.Sprintf(format, args...)...)
}
func (s *stringBuilder) String() string { return string(s.b) }

func sortedSubset(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
