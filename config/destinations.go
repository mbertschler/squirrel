package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
)

// destSchema declares the parameter schema for one destination type. The
// schema drives validation (required/optional fields, secret handling) and
// the rclone.conf rendering — for rclone-backed types, every param key here
// that resolves to a non-empty value is written verbatim to rclone.conf,
// except for "root" which is squirrel's own concept (we use it to compose
// the destination URI passed to rclone, not as an rclone backend param).
// Types with an empty rcloneType render no section, so their params stay
// out of rclone.conf entirely.
type destSchema struct {
	// rcloneType is the value written as `type = ...` in rclone.conf for
	// this destination. Empty means "no rclone remote" (the local backend
	// is addressed by absolute path; kopia drives its own binary).
	rcloneType string
	// requiredString fields must be present as plain strings and non-empty.
	requiredString []string
	// optionalString fields are passed through if set; absent is fine.
	optionalString []string
	// secretFields accept either a string literal or an inline table
	// { env = "VAR" }. The resolved literal is written to rclone.conf.
	secretFields []string
	// requiredSecret fields accept the same forms as secretFields but
	// must resolve to a non-empty value.
	requiredSecret []string
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
		// known_hosts_file points rclone at a known_hosts file so it
		// validates the server's host key before transferring; absent, rclone
		// accepts whatever host key the server presents. host_key_algorithms
		// pins the accepted host-key algorithms (rclone's space-separated
		// list). Both map straight to the rclone sftp options of the same
		// name. The unknown-field check confines them to this type.
		rcloneType:     "sftp",
		requiredString: []string{"host", "user"},
		optionalString: []string{"port", "key_file", "known_hosts_file", "host_key_algorithms"},
		secretFields:   []string{"password"},
	},
	"s3": {
		// storage_class maps to rclone's s3 storage_class config key; its
		// accepted values are whatever the backend supports (commonly
		// STANDARD and various archive tiers). The unknown-field check
		// confines it to this type.
		rcloneType:     "s3",
		requiredString: []string{"provider", "bucket"},
		optionalString: []string{"region", "endpoint", "storage_class"},
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
	"kopia": {
		// root is the local filesystem path of the kopia repository.
		// The password unlocks the repository (and creates it on first
		// use); kopia encrypts the repository contents itself, which is
		// also why a crypt block is rejected for this type.
		// verify_files_percent is the fraction of snapshot file bytes
		// `kopia snapshot verify` reads back when this destination gates
		// offload (default applied by the kopia handler when unset).
		rcloneType:     "",
		optionalString: []string{"verify_files_percent"},
		requiredSecret: []string{"password"},
	},
}

// SupportedTypes returns the sorted list of destination types squirrel
// supports. Used by error messages so users see what they could have
// typed.
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
	crypt, err := resolveCrypt(raw, typ)
	if err != nil {
		return nil, err
	}
	layout, err := resolveLayout(raw, typ)
	if err != nil {
		return nil, err
	}
	hashAlgo, err := resolveHashAlgo(raw, typ, layout)
	if err != nil {
		return nil, err
	}
	checkers, err := resolveCheckers(raw, typ)
	if err != nil {
		return nil, err
	}
	pathStyle, err := resolvePathStyle(raw, typ)
	if err != nil {
		return nil, err
	}
	pack, err := resolvePackKnobs(raw, layout)
	if err != nil {
		return nil, err
	}
	verifyEvery, err := resolveVerifyEvery(raw, layout)
	if err != nil {
		return nil, err
	}
	params, err := validateAndResolveParams(schema, raw)
	if err != nil {
		return nil, err
	}
	return &Destination{
		Name: name, Type: typ, Root: root, Layout: layout, Params: params,
		Crypt: crypt, HashAlgo: hashAlgo, Checkers: checkers, PathStyle: pathStyle,
		PackThreshold: pack.threshold, PackSize: pack.size, ZstdLevel: pack.zstdLevel,
		VerifyEvery: verifyEvery,
	}, nil
}

// resolveVerifyEvery validates the optional per-destination `verify_every`
// cadence that drives the agent's scheduled re-check of this destination's
// recorded objects and packs (the same pass as `squirrel verify`). It is
// meaningful only on the content-addressed and packed layouts that keep
// per-artifact fingerprints, so a present key on any other layout is
// rejected rather than silently ignored — a mirror destination has nothing
// for verify to re-check. Empty stays zero: no per-destination cadence, an
// [agent] verify_every default may still apply.
func resolveVerifyEvery(raw map[string]any, layout string) (time.Duration, error) {
	v, err := optionalString(raw, "verify_every")
	if err != nil {
		return 0, err
	}
	if v == "" {
		return 0, nil
	}
	if layout != LayoutContentAddressed && layout != LayoutPacked {
		return 0, fmt.Errorf("verify_every requires the %q or %q layout; layout %q keeps no per-object fingerprints to re-check", LayoutContentAddressed, LayoutPacked, layout)
	}
	return parseVolumeCadence("verify_every", v)
}

// sftpHashAlgos are the checksum types rclone's sftp backend can read
// via a server-side sum command, the valid values for `hash_algo`.
var sftpHashAlgos = map[string]bool{
	"md5": true, "sha1": true, "sha256": true, "crc32": true,
	"blake3": true, "xxh3": true, "xxh128": true,
}

// resolveHashAlgo validates the optional `hash_algo` key. sftp is the
// one backend where rclone must be told which server-side hash command
// to run; every other type exposes a fixed checksum, so the key is
// rejected there. Content-addressed sftp destinations default to
// "sha256" so scan-back fingerprints get a strong checksum without
// relying on rclone's md5/sha1 preference.
func resolveHashAlgo(raw map[string]any, typ, layout string) (string, error) {
	v, err := optionalString(raw, "hash_algo")
	if err != nil {
		return "", err
	}
	if v == "" {
		if typ == "sftp" && layout == LayoutContentAddressed {
			return "sha256", nil
		}
		return "", nil
	}
	if typ != "sftp" {
		return "", fmt.Errorf(`hash_algo is only supported on type "sftp" destinations; type %q exposes a fixed checksum`, typ)
	}
	if !sftpHashAlgos[v] {
		return "", fmt.Errorf("unknown hash_algo %q (supported: %v)", v, sortedKeys(sftpHashAlgos))
	}
	return v, nil
}

// resolveCheckers validates the optional `checkers` key: a positive
// integer cap on rclone's concurrent checkers for this destination.
func resolveCheckers(raw map[string]any, typ string) (int, error) {
	v, ok := raw["checkers"]
	if !ok {
		return 0, nil
	}
	switch typ {
	case "local", "kopia":
		return 0, fmt.Errorf("checkers requires an rclone-remote destination type, not %q", typ)
	}
	n, isInt := v.(int64)
	if !isInt || n <= 0 {
		return 0, errors.New("checkers must be a positive integer")
	}
	return int(n), nil
}

// resolvePathStyle validates the optional `force_path_style` key: a
// boolean confined to s3 destinations, consumed by the direct S3 ETag
// client (not rendered into rclone.conf — it governs only squirrel's own
// listing). An absent key is false.
func resolvePathStyle(raw map[string]any, typ string) (bool, error) {
	v, ok := raw["force_path_style"]
	if !ok {
		return false, nil
	}
	if typ != "s3" {
		return false, fmt.Errorf(`force_path_style is only supported on type "s3" destinations, not %q`, typ)
	}
	b, ok := v.(bool)
	if !ok {
		return false, errors.New("force_path_style must be a boolean")
	}
	return b, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolveLayout validates the optional `layout` key of a destination. An
// absent key resolves to LayoutMirror. LayoutContentAddressed and
// LayoutPacked drive squirrel's own rclone transfers, so both require an
// rclone-remote type: type "local" is addressed by filesystem path, and
// "kopia" repositories already use kopia's own content-addressed format.
func resolveLayout(raw map[string]any, typ string) (string, error) {
	v, err := optionalString(raw, "layout")
	if err != nil {
		return "", err
	}
	switch v {
	case "", LayoutMirror:
		return LayoutMirror, nil
	case LayoutContentAddressed, LayoutPacked:
		if err := requireRcloneRemote(v, typ); err != nil {
			return "", err
		}
		return v, nil
	default:
		return "", fmt.Errorf("unknown layout %q (supported: %q, %q, %q)", v, LayoutMirror, LayoutContentAddressed, LayoutPacked)
	}
}

// requireRcloneRemote rejects the two rclone-remote-only layouts on the
// destination types that can't drive per-object squirrel transfers: type
// "local" is a filesystem path, and "kopia" runs its own content-addressed
// binary. Shared by the LayoutContentAddressed and LayoutPacked branches so
// both give the same guardrail message.
func requireRcloneRemote(layout, typ string) error {
	switch typ {
	case "local":
		return fmt.Errorf(`layout %q requires an rclone-remote destination; type "local" is addressed by filesystem path`, layout)
	case "kopia":
		return fmt.Errorf(`layout %q requires an rclone-remote destination; type "kopia" repositories are content-addressed by kopia itself`, layout)
	}
	return nil
}

// Pack-layout knob defaults, applied when a LayoutPacked destination omits
// the corresponding key. Sizes are in bytes.
const (
	defaultPackThreshold = 32 << 20  // 32 MiB
	defaultPackSize      = 512 << 20 // 512 MiB
	defaultZstdLevel     = 3
	// minZstdLevel/maxZstdLevel bound zstd_level to the range
	// klauspost/compress exposes: 1 (fastest) .. 4 (best).
	minZstdLevel = 1
	maxZstdLevel = 4
)

// packKnobs carries the resolved pack-layout tuning for one destination.
type packKnobs struct {
	threshold int64
	size      int64
	zstdLevel int
}

// resolvePackKnobs validates the optional pack_threshold, pack_size, and
// zstd_level keys. They tune only the packed layout, so on any other layout
// a present key is an error (matching how hash_algo/checkers are confined to
// their types); LayoutPacked destinations get the defaults when a key is
// absent. Sizes are human strings like "32MiB"; zstd_level is an integer in
// [minZstdLevel, maxZstdLevel].
func resolvePackKnobs(raw map[string]any, layout string) (packKnobs, error) {
	if layout != LayoutPacked {
		for _, k := range []string{"pack_threshold", "pack_size", "zstd_level"} {
			if _, ok := raw[k]; ok {
				return packKnobs{}, fmt.Errorf("%s requires the %q layout", k, LayoutPacked)
			}
		}
		return packKnobs{}, nil
	}
	threshold, err := optionalSize(raw, "pack_threshold", defaultPackThreshold)
	if err != nil {
		return packKnobs{}, err
	}
	size, err := optionalSize(raw, "pack_size", defaultPackSize)
	if err != nil {
		return packKnobs{}, err
	}
	level, err := optionalZstdLevel(raw, "zstd_level", defaultZstdLevel)
	if err != nil {
		return packKnobs{}, err
	}
	return packKnobs{threshold: threshold, size: size, zstdLevel: level}, nil
}

// optionalSize parses a human size string (e.g. "32MiB") at key, returning
// def when the key is absent. The parsed value must be positive; zero or a
// negative/unparseable size is an error.
func optionalSize(raw map[string]any, key string, def int64) (int64, error) {
	v, ok := raw[key]
	if !ok {
		return def, nil
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return 0, fmt.Errorf(`%s must be a size string like "32MiB"`, key)
	}
	n, err := humanize.ParseBytes(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid size %q: %w", key, s, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("%s must be greater than zero", key)
	}
	if n > math.MaxInt64 {
		return 0, fmt.Errorf("%s: size %q is too large", key, s)
	}
	return int64(n), nil
}

// optionalZstdLevel parses the integer zstd_level at key, returning def when
// the key is absent. The value must fall within [minZstdLevel, maxZstdLevel].
func optionalZstdLevel(raw map[string]any, key string, def int) (int, error) {
	v, ok := raw[key]
	if !ok {
		return def, nil
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	if n < minZstdLevel || n > maxZstdLevel {
		return 0, fmt.Errorf("%s must be between %d and %d (klauspost zstd fastest..best), got %d", key, minZstdLevel, maxZstdLevel, n)
	}
	return int(n), nil
}

// resolveCrypt validates the optional `crypt` sub-table of a destination.
// A missing key yields nil (no encryption overlay). The two password
// fields go through the same secret resolution as destination credentials;
// password is required, password2 (the salt) is optional.
func resolveCrypt(raw map[string]any, typ string) (*Crypt, error) {
	v, ok := raw["crypt"]
	if !ok {
		return nil, nil
	}
	switch typ {
	case "local":
		return nil, errors.New(`crypt requires an rclone-remote destination; type "local" is addressed by filesystem path`)
	case "kopia":
		return nil, errors.New(`crypt requires an rclone-remote destination; type "kopia" repositories are encrypted by kopia itself`)
	}
	table, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("crypt must be a table, e.g. [destinations.<name>.crypt]")
	}
	password, err := resolveSecret(table, "password")
	if err != nil {
		return nil, fmt.Errorf("crypt: %w", err)
	}
	if password == "" {
		return nil, errors.New("crypt.password is required (rclone-obscured; generate with `rclone obscure`)")
	}
	password2, err := resolveSecret(table, "password2")
	if err != nil {
		return nil, fmt.Errorf("crypt: %w", err)
	}
	for k := range table {
		if k != "password" && k != "password2" {
			return nil, fmt.Errorf("crypt: unknown field %q", k)
		}
	}
	return &Crypt{Password: password, Password2: password2}, nil
}

// validateCryptRemoteNames rejects a config where one destination's crypt
// remote name is itself a declared destination — both would render an
// rclone.conf section under the same name, and rclone would resolve the
// shared name to whichever section comes last.
func validateCryptRemoteNames(dests map[string]*Destination) error {
	names := make([]string, 0, len(dests))
	for name := range dests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		d := dests[name]
		if d.Crypt == nil {
			continue
		}
		if _, clash := dests[d.CryptRemoteName()]; clash {
			return fmt.Errorf("destinations.%s: crypt remote name %q is already taken by another destination — rename one of them", name, d.CryptRemoteName())
		}
	}
	return nil
}

// validateAndResolveParams walks the schema, pulling each declared field
// out of raw and (for secrets) resolving { env = "..." } references. After
// the walk, any keys still in raw beyond {type, root, schema-known} are
// unknown fields and surface as an error — strictness keeps typos from
// silently disabling a field at rclone time.
func validateAndResolveParams(schema destSchema, raw map[string]any) (map[string]string, error) {
	out := make(map[string]string)
	seen := map[string]bool{
		"type": true, "root": true, "crypt": true, "layout": true,
		"hash_algo": true, "checkers": true, "force_path_style": true,
		"pack_threshold": true, "pack_size": true, "zstd_level": true,
		"verify_every": true,
	}
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
	for _, key := range schema.requiredSecret {
		v, err := resolveSecret(raw, key)
		if err != nil {
			return nil, err
		}
		if v == "" {
			return nil, fmt.Errorf("%s is required", key)
		}
		out[key] = v
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
// A destination with a crypt block renders two sections: the underlying
// remote exactly as without crypt, then the crypt overlay wrapping it.
// Each rendered section ends with a trailing newline, so sections
// concatenate directly into a valid rclone.conf.
func (d *Destination) RcloneSection() string {
	schema := destSchemas[d.Type]
	if schema.rcloneType == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", d.Name)
	fmt.Fprintf(&b, "type = %s\n", schema.rcloneType)
	// Stable ordering: required → optional → secret, alphabetical within
	// each band. Stable output makes the rendered file diffable.
	for _, key := range sortedSubset(schema.requiredString) {
		if v, ok := d.Params[key]; ok {
			fmt.Fprintf(&b, "%s = %s\n", key, v)
		}
	}
	for _, key := range sortedSubset(schema.optionalString) {
		if v, ok := d.Params[key]; ok {
			fmt.Fprintf(&b, "%s = %s\n", key, v)
		}
	}
	if d.Type == "sftp" {
		// rclone's sftp backend only autodetects md5sum/sha1sum, so BLAKE3
		// must be named explicitly or squirrel's `--hash blake3` syncs abort
		// with "hash type not supported". b3sum is the canonical BLAKE3 CLI
		// and must be on the remote's PATH.
		fmt.Fprintf(&b, "blake3sum_command = b3sum\n")
		if d.HashAlgo != "" {
			fmt.Fprintf(&b, "hashes = %s\n", d.HashAlgo)
		}
	}
	for _, key := range sortedSubset(schema.secretFields) {
		if v, ok := d.Params[key]; ok {
			fmt.Fprintf(&b, "%s = %s\n", key, v)
		}
	}
	if d.Crypt != nil {
		b.WriteString("\n")
		b.WriteString(d.cryptSection())
	}
	return b.String()
}

// CryptRemoteName is the rclone.conf section name of the crypt overlay
// stacked on this destination. Meaningful only when Crypt is non-nil.
func (d *Destination) CryptRemoteName() string {
	return d.Name + "-crypt"
}

// RemoteRoot is the path prefix inside the destination's rclone remote
// where this destination's tree starts. For bucket-addressed backends
// (s3, b2, gcs) that is the bucket joined with the configured root:
// rclone addresses the bucket as the leading path segment, while
// squirrel's config keeps `bucket` and `root` as separate keys — a
// composition that omits the bucket makes rclone treat the first
// segment of root (or the volume name) as the bucket name and silently
// write into the wrong bucket. Other backends use root verbatim (an
// sftp root may be deliberately absolute). path.Join cleans a "/" root
// to the bare bucket.
func (d *Destination) RemoteRoot() string {
	switch d.Type {
	case "s3", "b2", "gcs":
		return path.Join(d.Params["bucket"], d.Root)
	}
	return d.Root
}

// cryptSection renders the crypt overlay remote. Its remote line bakes the
// destination root in, so transfers through the overlay address
// volume-relative paths directly. filename_encryption is fixed off: the
// overlay encrypts file contents only, and the destination keeps the same
// browsable tree layout as an unencrypted destination.
func (d *Destination) cryptSection() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", d.CryptRemoteName())
	b.WriteString("type = crypt\n")
	fmt.Fprintf(&b, "remote = %s:%s\n", d.Name, d.RemoteRoot())
	b.WriteString("filename_encryption = off\n")
	b.WriteString("directory_name_encryption = false\n")
	fmt.Fprintf(&b, "password = %s\n", d.Crypt.Password)
	if d.Crypt.Password2 != "" {
		fmt.Fprintf(&b, "password2 = %s\n", d.Crypt.Password2)
	}
	return b.String()
}

func sortedSubset(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// CanEverGateOffload reports whether a durability push to this destination
// can ever advance its vector with a component the offload gate will accept
// — one that is content-verified, either natively or after a scan-back
// fingerprint upgrades a presence+size component. When it returns false,
// reason names the structural gap for the caller's message; it is empty
// when capable.
//
// The one structurally-incapable shape is a mirror-layout destination
// behind a crypt overlay: crypt remotes expose no content hash, so rclone
// falls back to a size+mtime comparison (never BLAKE3 — see
// sync.EffectiveShallow), and the mirror layout records no scan-back
// fingerprint that a later `squirrel verify` could upgrade. The
// content-addressed and packed layouts, by contrast, advance with
// presence+size but stay upgradable — their fingerprint is read back over
// the stored ciphertext, so a crypt overlay does not block it — and every
// native content-verified destination (a plain mirror's BLAKE3, kopia's own
// verification) is capable outright.
func (d *Destination) CanEverGateOffload() (bool, string) {
	switch d.Layout {
	case LayoutContentAddressed, LayoutPacked:
		return true, ""
	}
	if d.Crypt != nil {
		return false, "shallow path-mirrored crypt destination: BLAKE3 verification cannot pass through the crypt overlay and the mirror layout records no scan-back fingerprint to upgrade it"
	}
	return true, ""
}
