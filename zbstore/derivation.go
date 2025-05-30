// Copyright 2024 The zb Authors
// SPDX-License-Identifier: MIT

package zbstore

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"iter"
	"maps"
	"slices"
	"strings"

	"zb.256lights.llc/pkg/internal/aterm"
	"zb.256lights.llc/pkg/internal/xmaps"
	"zb.256lights.llc/pkg/sets"
	"zombiezen.com/go/nix"
	"zombiezen.com/go/nix/nar"
)

// DerivationExt is the file extension for a marshalled [Derivation].
const DerivationExt = ".drv"

// A Derivation represents a store derivation:
// a single, specific, constant build action.
type Derivation struct {
	// Dir is the store directory this derivation is a part of.
	Dir Directory

	// Name is the human-readable name of the derivation,
	// i.e. the part after the digest in the store object name.
	Name string
	// System is a string representing the OS and architecture tuple
	// that this derivation is intended to run on.
	System string
	// Builder is the path to the program to run the build.
	Builder string
	// Args is the list of arguments that should be passed to the builder program.
	Args []string
	// Env is the environment variables that should be passed to the builder program.
	Env map[string]string

	// InputSources is the set of source filesystem objects that this derivation depends on.
	InputSources sets.Sorted[Path]
	// InputDerivations is the set of derivations that this derivation depends on.
	// The mapped values are the set of output names that are used.
	InputDerivations map[Path]*sets.Sorted[string]
	// Outputs is the set of outputs that the derivation produces.
	Outputs map[string]*DerivationOutputType
}

// ParseDerivation parses a derivation from ATerm format.
// name should be the derivation's name as returned by [Path.DerivationName].
func ParseDerivation(dir Directory, name string, data []byte) (*Derivation, error) {
	drv := &Derivation{
		Dir:  dir,
		Name: name,
	}
	var ok bool
	data, ok = bytes.CutPrefix(data, []byte("Derive"))
	if !ok {
		return nil, fmt.Errorf("parse %s derivation: 'Derive' constructor not found", drv.Name)
	}
	r := bytes.NewReader(data)
	if err := drv.parseTuple(aterm.NewScanner(r)); err != nil {
		return nil, err
	}
	if r.Len() > 0 {
		return nil, fmt.Errorf("parse %s derivation: trailing data", drv.Name)
	}
	return drv, nil
}

// Export marshals the derivation to a NAR containing ATerm format
// and computes the derivation's store metadata using the given hashing algorithm.
//
// At the moment, the only supported algorithm is [nix.SHA256].
func (drv *Derivation) Export(hashType nix.HashType) ([]byte, *ExportTrailer, error) {
	if drv.Name == "" {
		return nil, nil, fmt.Errorf("export derivation: missing name")
	}
	if drv.Dir == "" {
		return nil, nil, fmt.Errorf("export derivation %s: missing store directory", drv.Name)
	}

	drvBytes, err := drv.MarshalText()
	if err != nil {
		return nil, nil, err
	}
	narBuffer := new(bytes.Buffer)
	narHasher := nix.NewHasher(hashType)
	nw := nar.NewWriter(io.MultiWriter(narHasher, narBuffer))
	if err := nw.WriteHeader(&nar.Header{Size: int64(len(drvBytes))}); err != nil {
		return nil, nil, fmt.Errorf("export derivation %s: %v", drv.Name, err)
	}
	if _, err := nw.Write(drvBytes); err != nil {
		return nil, nil, fmt.Errorf("export derivation %s: %v", drv.Name, err)
	}
	if err := nw.Close(); err != nil {
		return nil, nil, fmt.Errorf("export derivation %s: %v", drv.Name, err)
	}

	caHasher := nix.NewHasher(hashType)
	caHasher.Write(drvBytes)
	trailer := &ExportTrailer{
		ContentAddress: nix.TextContentAddress(caHasher.SumHash()),
		References:     drv.References().Others,
	}
	trailer.StorePath, err = FixedCAOutputPath(
		drv.Dir,
		drv.Name+DerivationExt,
		trailer.ContentAddress,
		drv.References(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("export derivation %s: %v", drv.Name, err)
	}
	return narBuffer.Bytes(), trailer, nil
}

// Clone returns a deep copy of drv.
func (drv *Derivation) Clone() *Derivation {
	drvClone := &Derivation{
		Dir:          drv.Dir,
		Name:         drv.Name,
		System:       drv.System,
		Builder:      drv.Builder,
		Args:         slices.Clone(drv.Args),
		Env:          maps.Clone(drv.Env),
		InputSources: *drv.InputSources.Clone(),
		Outputs:      maps.Clone(drv.Outputs),
	}
	if drv.InputDerivations != nil {
		drvClone.InputDerivations = make(map[Path]*sets.Sorted[string], len(drv.InputDerivations))
		for drvPath, outputNames := range drv.InputDerivations {
			drvClone.InputDerivations[drvPath] = outputNames.Clone()
		}
	}
	return drvClone
}

// InputDerivationOutputs returns an iterator over the output references
// this derivation uses as inputs.
// The iterator will produce references in lexicographic order of the derivation path,
// then in lexicographic order of the output name within a derivation path.
func (drv *Derivation) InputDerivationOutputs() iter.Seq[OutputReference] {
	return func(yield func(OutputReference) bool) {
		for inputDrvPath, inputOutputNames := range xmaps.Sorted(drv.InputDerivations) {
			for _, inputOutputName := range inputOutputNames.All() {
				ref := OutputReference{
					DrvPath:    inputDrvPath,
					OutputName: inputOutputName,
				}
				if !yield(ref) {
					return
				}
			}
		}
	}
}

// References returns the set of other store paths that the derivation references.
// Derivations will never have a self-reference.
func (drv *Derivation) References() References {
	refs := References{}
	refs.Others.Grow(drv.InputSources.Len() + len(drv.InputDerivations))
	refs.Others.AddSet(&drv.InputSources)
	for input := range drv.InputDerivations {
		refs.Others.Add(input)
	}
	return refs
}

// OutputPath returns a fixed output's store object path.
// OutputPath returns an error if the output's path cannot be known ahead of realization.
func (drv *Derivation) OutputPath(outputName string) (Path, error) {
	outputType, ok := drv.Outputs[outputName]
	if !ok {
		return "", fmt.Errorf("output path for %s: no such output", outputName)
	}
	return derivationOutputPath(drv.Dir, drv.Name, outputName, outputType)
}

// derivationOutputPath returns a fixed output's store object path
// for the given store (e.g. "/opt/zb/store"),
// derivation name (e.g. "hello"),
// and output name (e.g. "out").
func derivationOutputPath(store Directory, drvName, outputName string, t *DerivationOutputType) (Path, error) {
	if t == nil {
		return "", fmt.Errorf("output path for %s: non-fixed output type", outputName)
	}
	switch t.typ {
	case fixedCAOutputType:
		if outputName != DefaultDerivationOutputName {
			drvName += "-" + outputName
		}
		return FixedCAOutputPath(store, drvName, t.ca, References{})
	default:
		return "", fmt.Errorf("output path for %s: non-fixed output type", outputName)
	}
}

// MarshalText converts the derivation to ATerm format.
func (drv *Derivation) MarshalText() ([]byte, error) {
	if drv.Name == "" {
		return nil, fmt.Errorf("marshal derivation: missing name")
	}
	if drv.Dir == "" {
		return nil, fmt.Errorf("marshal %s derivation: missing store directory", drv.Name)
	}

	var buf []byte
	buf = append(buf, "Derive(["...)
	for i, outName := range xmaps.SortedKeys(drv.Outputs) {
		if i > 0 {
			buf = append(buf, ',')
		}
		if !IsValidOutputName(outName) {
			return nil, fmt.Errorf("marshal %s derivation: invalid output name %q", drv.Name, outName)
		}
		var err error
		buf, err = drv.Outputs[outName].marshalText(buf, drv.Dir, drv.Name, outName)
		if err != nil {
			return nil, fmt.Errorf("marshal %s derivation: %v", drv.Name, err)
		}
	}

	buf = append(buf, "],["...)
	for drvPath := range drv.InputDerivations {
		if got := drvPath.Dir(); got != drv.Dir {
			return nil, fmt.Errorf("marshal %s derivation: inputs: unexpected store directory %s (using %s)",
				drv.Name, got, drv.Dir)
		}
	}
	buf = marshalInputDerivations(buf, drv.InputDerivations)

	buf = append(buf, "],["...)
	for i, src := range drv.InputSources.All() {
		if i > 0 {
			buf = append(buf, ',')
		}
		if got := src.Dir(); got != drv.Dir {
			return nil, fmt.Errorf("marshal %s derivation: inputs: unexpected store directory %s (using %s)",
				drv.Name, got, drv.Dir)
		}
		buf = aterm.AppendString(buf, string(src))
	}

	buf = append(buf, "],"...)
	buf = aterm.AppendString(buf, drv.System)
	buf = append(buf, ","...)
	buf = aterm.AppendString(buf, drv.Builder)

	buf = append(buf, ",["...)
	for i, arg := range drv.Args {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = aterm.AppendString(buf, arg)
	}

	buf = append(buf, "],["...)
	for i, k := range xmaps.SortedKeys(drv.Env) {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '(')
		buf = aterm.AppendString(buf, k)
		buf = append(buf, ',')
		buf = aterm.AppendString(buf, drv.Env[k])
		buf = append(buf, ')')
	}

	buf = append(buf, "])"...)

	return buf, nil
}

func marshalInputDerivations[K ~string](buf []byte, m map[K]*sets.Sorted[string]) []byte {
	for i, k := range xmaps.SortedKeys(m) {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '(')
		buf = aterm.AppendString(buf, string(k))
		buf = append(buf, ",["...)
		outputs := m[k]
		for j, out := range outputs.All() {
			if j > 0 {
				buf = append(buf, ',')
			}
			buf = aterm.AppendString(buf, out)
		}
		buf = append(buf, "])"...)
	}
	return buf
}

func (drv *Derivation) parseTuple(s *aterm.Scanner) error {
	if _, err := expectToken(s, aterm.LParen); err != nil {
		return fmt.Errorf("parse %s derivation: %v", drv.Name, err)
	}

	// Parse outputs.
	if _, err := expectToken(s, aterm.LBracket); err != nil {
		return fmt.Errorf("parse %s derivation: outputs: %v", drv.Name, err)
	}
	drv.Outputs = xmaps.Init(drv.Outputs)
	for {
		tok, err := s.ReadToken()
		if err != nil {
			return err
		}
		if tok.Kind == aterm.RBracket {
			break
		}
		s.UnreadToken()

		outName, outType, err := parseDerivationOutputType(s)
		if err != nil {
			return fmt.Errorf("parse %s derivation: %v", drv.Name, err)
		}
		if _, ok := drv.Outputs[outName]; ok {
			return fmt.Errorf("parse %s derivation: multiple outputs named %q", drv.Name, outName)
		}
		drv.Outputs[outName] = outType
	}

	// Parse input derivations.
	if _, err := expectToken(s, aterm.LBracket); err != nil {
		return fmt.Errorf("parse %s derivation: input derivations: %v", drv.Name, err)
	}
	drv.InputDerivations = xmaps.Init(drv.InputDerivations)
	for {
		tok, err := s.ReadToken()
		if err != nil {
			return err
		}
		if tok.Kind == aterm.RBracket {
			break
		}
		s.UnreadToken()

		drvPath, outputNames, err := parseInputDerivation(s)
		if err != nil {
			return fmt.Errorf("parse %s derivation: %v", drv.Name, err)
		}
		if drvPath.Dir() != drv.Dir {
			return fmt.Errorf("parse %s derivation: input derivation %s not in directory %s", drv.Name, drvPath, drv.Dir)
		}
		if _, ok := drv.InputDerivations[drvPath]; ok {
			return fmt.Errorf("parse %s derivation: multiple input derivations for %s", drv.Name, drvPath)
		}
		drv.InputDerivations[drvPath] = outputNames
	}

	// Parse input sources.
	drv.InputSources.Clear()
	err := parseStringList(s, func(val string) error {
		p, err := ParsePath(val)
		if err != nil {
			return err
		}
		drv.InputSources.Add(p)
		return nil
	})
	if err != nil {
		return fmt.Errorf("parse %s derivation: input sources: %v", drv.Name, err)
	}

	// Parse system.
	tok, err := expectToken(s, aterm.String)
	if err != nil {
		return fmt.Errorf("parse %s derivation: system: %v", drv.Name, err)
	}
	drv.System = tok.Value

	// Parse builder.
	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return fmt.Errorf("parse %s derivation: builder: %v", drv.Name, err)
	}
	drv.Builder = tok.Value

	// Parse builder arguments.
	drv.Args = slices.Delete(drv.Args, 0, len(drv.Args))
	err = parseStringList(s, func(arg string) error {
		drv.Args = append(drv.Args, arg)
		return nil
	})
	if err != nil {
		return fmt.Errorf("parse %s derivation: builder args: %v", drv.Name, err)
	}

	// Parse environment.
	if err := drv.parseEnv(s); err != nil {
		return err
	}

	if _, err := expectToken(s, aterm.RParen); err != nil {
		return fmt.Errorf("parse %s derivation: %v", drv.Name, err)
	}
	return nil
}

func parseInputDerivation(s *aterm.Scanner) (drvPath Path, outputNames *sets.Sorted[string], err error) {
	if _, err := expectToken(s, aterm.LParen); err != nil {
		return "", nil, fmt.Errorf("parse input derivation: %v", err)
	}

	tok, err := expectToken(s, aterm.String)
	if err != nil {
		return "", nil, fmt.Errorf("parse input derivation: name: %v", err)
	}
	drvPathString := tok.Value

	outputNames = new(sets.Sorted[string])
	err = parseStringList(s, func(val string) error {
		outputNames.Add(val)
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("parse input derivation %s: output names: %v", drvPathString, err)
	}

	if _, err := expectToken(s, aterm.RParen); err != nil {
		return "", nil, fmt.Errorf("parse input derivation %s: %v", drvPathString, err)
	}

	drvPath, err = ParsePath(drvPathString)
	if err != nil {
		return "", nil, fmt.Errorf("parse input derivation %s: %v", drvPathString, err)
	}
	return drvPath, outputNames, nil
}

func (drv *Derivation) parseEnv(s *aterm.Scanner) error {
	if _, err := expectToken(s, aterm.LBracket); err != nil {
		return fmt.Errorf("parse %s derivation: env: expected '[', found %v", drv.Name, err)
	}
	drv.Env = xmaps.Init(drv.Env)
	for {
		tok, err := s.ReadToken()
		if err != nil {
			return fmt.Errorf("parse %s derivation: env: %v", drv.Name, err)
		}
		switch tok.Kind {
		case aterm.RBracket:
			return nil
		case aterm.LParen:
			// Carry on.
		default:
			return fmt.Errorf("parse %s derivation: env: expected ']' or '(', found %v", drv.Name, tok)
		}

		tok, err = expectToken(s, aterm.String)
		if err != nil {
			return fmt.Errorf("parse %s derivation: env: %v", drv.Name, err)
		}
		k := tok.Value
		if _, exists := drv.Env[k]; exists {
			return fmt.Errorf("parse %s derivation: env: multiple entries for %s", drv.Name, k)
		}

		tok, err = expectToken(s, aterm.String)
		if err != nil {
			return fmt.Errorf("parse %s derivation: env: %s: %v", drv.Name, k, err)
		}
		v := tok.Value

		if _, err := expectToken(s, aterm.RParen); err != nil {
			return fmt.Errorf("parse %s derivation: env: %s: %v", drv.Name, k, err)
		}

		drv.Env[k] = v
	}
}

// DefaultDerivationOutputName is the name of the primary output of a derivation.
// It is omitted in a number of contexts.
const DefaultDerivationOutputName = "out"

// IsValidOutputName reports whether the given string is valid as a derivation output name.
func IsValidOutputName(name string) bool {
	// TODO(someday): This should be an allow list of characters.
	return name != "" && !strings.ContainsAny(name, "^!")
}

type derivationOutputType int8

const (
	fixedCAOutputType derivationOutputType = 1 + iota
	floatingCAOutputType
)

// A DerivationOutputType describes the content addressing scheme of an output of a [Derivation].
type DerivationOutputType struct {
	typ      derivationOutputType
	ca       nix.ContentAddress
	method   contentAddressMethod
	hashAlgo nix.HashType
}

// FixedCAOutput returns a [DerivationOutputType]
// that must match the given content address assertion.
func FixedCAOutput(ca nix.ContentAddress) *DerivationOutputType {
	return &DerivationOutputType{
		typ: fixedCAOutputType,
		ca:  ca,
	}
}

// FlatFileFloatingCAOutput returns a [DerivationOutputType]
// that must be a single file
// and will be hashed with the given algorithm.
// The hash will not be known until the derivation is realized.
func FlatFileFloatingCAOutput(hashAlgo nix.HashType) *DerivationOutputType {
	return &DerivationOutputType{
		typ:      floatingCAOutputType,
		method:   flatFileIngestionMethod,
		hashAlgo: hashAlgo,
	}
}

// RecursiveFileFloatingCAOutput returns a [DerivationOutputType]
// that is hashed as a NAR with the given algorithm.
// The hash will not be known until the derivation is realized.
func RecursiveFileFloatingCAOutput(hashAlgo nix.HashType) *DerivationOutputType {
	return &DerivationOutputType{
		typ:      floatingCAOutputType,
		method:   recursiveFileIngestionMethod,
		hashAlgo: hashAlgo,
	}
}

// IsFixed reports whether the output was created by [FixedCAOutput].
func (t *DerivationOutputType) IsFixed() bool {
	if t == nil {
		return false
	}
	return t.typ == fixedCAOutputType
}

// IsFloating reports whether the output's content hash cannot be known
// until the derivation is realized.
// This is true for outputs returned by
// [FlatFileFloatingCAOutput] and [RecursiveFileFloatingCAOutput].
func (t *DerivationOutputType) IsFloating() bool {
	if t == nil {
		return false
	}
	return t.typ == floatingCAOutputType
}

// HashType returns the hash type of the derivation output, if present.
func (t *DerivationOutputType) HashType() (_ nix.HashType, ok bool) {
	switch {
	case t.IsFixed():
		return t.ca.Hash().Type(), true
	case t.IsFloating():
		return t.hashAlgo, true
	default:
		return 0, false
	}
}

// FixedCA returns a fixed hash output's content address.
// ok is true only if the output was created by [FixedCAOutput].
func (out *DerivationOutputType) FixedCA() (_ ContentAddress, ok bool) {
	if !out.IsFixed() {
		return ContentAddress{}, false
	}
	return out.ca, true
}

// IsRecursiveFile reports whether the derivation output
// uses recursive (NAR) hashing.
func (t *DerivationOutputType) IsRecursiveFile() bool {
	switch {
	case t.IsFixed():
		return t.ca.IsRecursiveFile()
	case t.IsFloating():
		return t.method == recursiveFileIngestionMethod
	default:
		return false
	}
}

func (t *DerivationOutputType) marshalText(dst []byte, storeDir Directory, drvName, outName string) ([]byte, error) {
	dst = append(dst, '(')
	dst = aterm.AppendString(dst, outName)
	if t == nil {
		dst = append(dst, `,"","","")`...)
		return dst, nil
	}
	switch t.typ {
	case fixedCAOutputType:
		dst = append(dst, ',')
		p, err := derivationOutputPath(storeDir, drvName, outName, t)
		if err != nil {
			return dst, fmt.Errorf("marshal %s output: %v", outName, err)
		}
		dst = aterm.AppendString(dst, string(p))
		dst = append(dst, ',')
		h := t.ca.Hash()
		dst = aterm.AppendString(dst, methodOfContentAddress(t.ca).prefix()+h.Type().String())
		dst = append(dst, ',')
		dst = aterm.AppendString(dst, h.RawBase16())
	case floatingCAOutputType:
		dst = append(dst, `,"",`...)
		dst = aterm.AppendString(dst, t.method.prefix()+t.hashAlgo.String())
		dst = append(dst, `,""`...)
	default:
		return dst, fmt.Errorf("marshal %s output: invalid type %v", outName, t.typ)
	}
	dst = append(dst, ')')
	return dst, nil
}

func parseDerivationOutputType(s *aterm.Scanner) (outName string, out *DerivationOutputType, err error) {
	tok, err := expectToken(s, aterm.LParen)
	if err != nil {
		return "", nil, fmt.Errorf("parse output: %v", err)
	}

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return "", nil, fmt.Errorf("parse output: name: %v", err)
	}
	outName = tok.Value
	if !IsValidOutputName(outName) {
		return "", nil, fmt.Errorf("parse output: name: invalid name %q", outName)
	}

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return "", nil, fmt.Errorf("parse %s output: path: %v", outName, err)
	}
	path := tok.Value

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return "", nil, fmt.Errorf("parse %s output: hash algorithm: %v", outName, err)
	}
	caInfo := tok.Value

	tok, err = expectToken(s, aterm.String)
	if err != nil {
		return "", nil, fmt.Errorf("parse %s output: hash: %v", outName, err)
	}
	hashHex := tok.Value

	if _, err := expectToken(s, aterm.RParen); err != nil {
		return "", nil, fmt.Errorf("parse %s output: %v", outName, err)
	}

	method, hashAlgo, err := parseHashAlgorithm(caInfo)
	if err != nil {
		return outName, nil, fmt.Errorf("parse %s output: hash algorithm: %v", outName, err)
	}
	hashBits, err := hex.DecodeString(hashHex)
	if err != nil {
		return outName, nil, fmt.Errorf("parse %s output: hash: %v", outName, err)
	}
	switch {
	case path == "" && hashHex == "":
		out = &DerivationOutputType{
			typ:      floatingCAOutputType,
			method:   method,
			hashAlgo: hashAlgo,
		}
	case hashHex != "":
		if got, want := len(hashBits), hashAlgo.Size(); got != want {
			err = fmt.Errorf("parse %s output: hash: incorrect size (got %d bytes but %v uses %d)",
				outName, got, hashAlgo, want)
			return outName, nil, err
		}
		h := nix.NewHash(hashAlgo, hashBits)
		switch method {
		case flatFileIngestionMethod:
			out = FixedCAOutput(nix.FlatFileContentAddress(h))
		case recursiveFileIngestionMethod:
			out = FixedCAOutput(nix.RecursiveFileContentAddress(h))
		case textIngestionMethod:
			out = FixedCAOutput(nix.TextContentAddress(h))
		default:
			return outName, nil, fmt.Errorf("parse %s output: unhandled hash algorithm %q", outName, caInfo)
		}
	default:
		return outName, nil, fmt.Errorf("parse %s output: unknown type", outName)
	}
	return outName, out, nil
}

func parseHashAlgorithm(s string) (contentAddressMethod, nix.HashType, error) {
	method := flatFileIngestionMethod
	s, ok := strings.CutPrefix(s, "r:")
	if ok {
		method = recursiveFileIngestionMethod
	} else {
		s, ok = strings.CutPrefix(s, "text:")
		if ok {
			method = textIngestionMethod
		}
	}

	typ, err := nix.ParseHashType(s)
	if err != nil {
		return method, 0, err
	}
	return method, typ, nil
}

// OutputReference is a reference to an output of a derivation.
type OutputReference struct {
	DrvPath    Path
	OutputName string
}

// ParseOutputReference parses the result of [OutputReference.String]
// back into an OutputReference.
func ParseOutputReference(s string) (OutputReference, error) {
	i := strings.LastIndexByte(s, '!')
	if i < 0 {
		return OutputReference{}, fmt.Errorf("parse output reference %q: missing '!' separator", s)
	}
	result := OutputReference{OutputName: s[i+1:]}
	if !IsValidOutputName(result.OutputName) {
		return OutputReference{}, fmt.Errorf("parse output reference %q: invalid output name %q", s, result.OutputName)
	}
	var err error
	result.DrvPath, err = ParsePath(s[:i])
	if err != nil {
		return OutputReference{}, fmt.Errorf("parse output reference %q: %v", s, err)
	}
	if _, isDrv := result.DrvPath.DerivationName(); !isDrv {
		return OutputReference{}, fmt.Errorf("parse output reference %q: not a derivation", s)
	}
	return result, nil
}

// IsZero reports whether the reference is the zero value.
func (ref OutputReference) IsZero() bool {
	return ref == OutputReference{}
}

// String returns the path and the output name separated by "!".
func (ref OutputReference) String() string {
	return string(ref.DrvPath) + "!" + ref.OutputName
}

// MarshalText returns the output reference in the same format as [OutputReference.String].
func (ref OutputReference) MarshalText() ([]byte, error) {
	if ref.DrvPath == "" {
		return nil, fmt.Errorf("marshal output reference: empty path")
	}
	if !IsValidOutputName(ref.OutputName) {
		return nil, fmt.Errorf("marshal output reference: invalid output name %q", ref.OutputName)
	}
	return []byte(ref.String()), nil
}

// UnmarshalText parses the output reference like [ParseOutputReference] into ref.
func (ref *OutputReference) UnmarshalText(text []byte) error {
	var err error
	*ref, err = ParseOutputReference(string(text))
	return err
}

// HashPlaceholder returns the placeholder string used in leiu of a derivation's output path.
// During a derivation's realization, the backend replaces any occurrences of the placeholder
// in the derivation's environment variables
// with the temporary output path (used until the content address stabilizes).
func HashPlaceholder(outputName string) string {
	h := nix.NewHasher(nix.SHA256)
	h.WriteString("nix-output:")
	h.WriteString(outputName)
	return "/" + h.SumHash().RawBase32()
}

// UnknownCAOutputPlaceholder returns the placeholder
// for an unknown output of a content-addressed derivation.
func UnknownCAOutputPlaceholder(ref OutputReference) string {
	// We accept non-".drv" paths here for simplicity,
	// so we don't use [Path.DerivationName].
	drvName := strings.TrimSuffix(ref.DrvPath.Name(), DerivationExt)

	h := nix.NewHasher(nix.SHA256)
	h.WriteString("nix-upstream-output:")
	h.WriteString(ref.DrvPath.Digest())
	h.WriteString(":")
	h.WriteString(drvName)
	if ref.OutputName != DefaultDerivationOutputName {
		h.WriteString("-")
		h.WriteString(ref.OutputName)
	}
	return "/" + h.SumHash().RawBase32()
}

func parseStringList(s *aterm.Scanner, f func(string) error) error {
	tok, err := expectToken(s, aterm.LBracket)
	if err != nil {
		return err
	}
	for {
		tok, err = s.ReadToken()
		if err != nil {
			return err
		}
		switch tok.Kind {
		case aterm.String:
			if err := f(tok.Value); err != nil {
				return err
			}
		case aterm.RBracket:
			return nil
		default:
			return fmt.Errorf("expected string or ']', found %v", tok)
		}
	}
}

func expectToken(s *aterm.Scanner, kind aterm.TokenKind) (aterm.Token, error) {
	tok, err := s.ReadToken()
	if err != nil {
		return aterm.Token{}, err
	}
	if tok.Kind != kind {
		var want string
		if kind == aterm.String {
			want = "string"
		} else {
			want = `'` + string(kind) + `'`
		}
		return tok, fmt.Errorf("expected %s, found %v", want, tok)
	}
	return tok, nil
}
