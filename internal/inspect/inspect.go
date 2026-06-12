// Package inspect analyses a regular terraform plan (resource_changes) and
// classifies nested block-level diffs as provider noise (optional attrs echoed
// back by the API, computed IDs) versus intentional semantic changes. It
// complements osmo's drift absorption by explaining why Terraform proposes
// delete+create churn even when no meaningful config change occurred.
package inspect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/pandey-raghvendra/osmo/internal/blockid"
)

// ---- Public output types ---------------------------------------------------

// Result is the full inspect output for one plan.
type Result struct {
	Resources []ResourceReport
	// AllSafe is true when every resource report is noise-only or has-additions.
	// Removals and semantic attr changes set AllSafe to false.
	AllSafe bool
}

// ResourceReport is the inspect result for one resource_change entry.
type ResourceReport struct {
	Address string
	Type    string
	Actions []string
	// NoiseBlocks: matched block instances whose entire diff is provider noise.
	NoiseBlocks []BlockNoise
	// AddedBlocks: new block instances added in config (intentional addition).
	AddedBlocks []BlockSummary
	// RemovedBlocks: block instances removed from config.
	RemovedBlocks []BlockSummary
	// ChangedBlocks: matched block instances with real semantic attr changes.
	ChangedBlocks []BlockChange
	// Verdict summarises the resource-level outcome.
	Verdict string // "noise-only" | "has-additions" | "has-removals" | "has-changes" | "mixed"
}

// BlockNoise is a matched nested block whose diff contains only provider noise.
type BlockNoise struct {
	BlockType string
	Key       string
	Attrs     []AttrDiff
}

// BlockSummary is a new or removed nested block instance.
type BlockSummary struct {
	BlockType string
	Key       string
	MainAttrs map[string]interface{}
}

// BlockChange is a matched nested block with at least one semantic attr change.
type BlockChange struct {
	BlockType     string
	Key           string
	SemanticDiffs []AttrDiff
	NoiseDiffs    []AttrDiff
}

// AttrDiff is a single attribute change with its noise classification.
type AttrDiff struct {
	Attr   string
	Before interface{}
	After  interface{}
	Noise  bool
	Reason string // non-empty when Noise == true
}

// ---- JSON plan types -------------------------------------------------------

type planJSON struct {
	ResourceChanges []planRC `json:"resource_changes"`
}

type planRC struct {
	Address string     `json:"address"`
	Type    string     `json:"type"`
	Name    string     `json:"name"`
	Change  changeBlob `json:"change"`
}

type changeBlob struct {
	Actions      []string               `json:"actions"`
	Before       map[string]interface{} `json:"before"`
	After        map[string]interface{} `json:"after"`
	AfterUnknown map[string]interface{} `json:"after_unknown"`
}

// ---- Entry point -----------------------------------------------------------

// Run parses a terraform show -json plan and classifies each resource change.
func Run(raw []byte, idreg *blockid.Registry, cfg NormalsConfig) (Result, error) {
	var pj planJSON
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&pj); err != nil {
		return Result{}, fmt.Errorf("parse plan json: %w", err)
	}
	nreg := buildNormalsRegistry(cfg)

	var reports []ResourceReport
	allSafe := true
	for _, rc := range pj.ResourceChanges {
		if noOpActions(rc.Change.Actions) {
			continue
		}
		report := classifyResource(rc, idreg, nreg)
		reports = append(reports, report)
		if report.Verdict == "has-removals" || report.Verdict == "has-changes" || report.Verdict == "mixed" {
			allSafe = false
		}
	}
	return Result{Resources: reports, AllSafe: allSafe}, nil
}

func noOpActions(actions []string) bool {
	for _, a := range actions {
		if a != "no-op" && a != "read" {
			return false
		}
	}
	return true
}

// ---- Resource classification -----------------------------------------------

func classifyResource(rc planRC, idreg *blockid.Registry, nreg *normalsRegistry) ResourceReport {
	r := ResourceReport{
		Address: rc.Address,
		Type:    rc.Type,
		Actions: rc.Change.Actions,
	}

	before := safeMap(rc.Change.Before)
	after := safeMap(rc.Change.After)
	afterUnknown := safeMap(rc.Change.AfterUnknown)

	for _, bt := range blockTypeSet(before, after) {
		beforeSlice := toSlice(before[bt])
		afterSlice := toSlice(after[bt])
		unknownSlice, entirelyUnknown := unknownSliceFor(afterUnknown, bt)

		idKeys := idreg.Keys(rc.Type, []string{bt})
		noise, added, removed, changed := classifyBlocks(
			rc.Type, bt,
			beforeSlice, afterSlice, unknownSlice, entirelyUnknown,
			idKeys, nreg,
		)
		r.NoiseBlocks = append(r.NoiseBlocks, noise...)
		r.AddedBlocks = append(r.AddedBlocks, added...)
		r.RemovedBlocks = append(r.RemovedBlocks, removed...)
		r.ChangedBlocks = append(r.ChangedBlocks, changed...)
	}

	r.Verdict = deriveVerdict(r)
	return r
}

func deriveVerdict(r ResourceReport) string {
	hasRemovals := len(r.RemovedBlocks) > 0
	hasChanges := len(r.ChangedBlocks) > 0
	hasAdditions := len(r.AddedBlocks) > 0
	hasNoise := len(r.NoiseBlocks) > 0

	switch {
	case (hasRemovals || hasChanges) && (hasAdditions || hasNoise):
		return "mixed"
	case hasChanges:
		return "has-changes"
	case hasRemovals:
		return "has-removals"
	case hasAdditions:
		return "has-additions"
	case hasNoise:
		return "noise-only"
	default:
		return "no-diff"
	}
}

// ---- Block classification --------------------------------------------------

func classifyBlocks(
	resType, blockType string,
	before, after, unknownSlice []interface{},
	entirelyUnknown bool,
	identityKeys []string,
	nreg *normalsRegistry,
) (noise []BlockNoise, added []BlockSummary, removed []BlockSummary, changed []BlockChange) {
	bMaps := toStateMaps(before)
	aMaps := toStateMaps(after)

	used := make([]bool, len(aMaps))
	for _, bm := range bMaps {
		bestScore, bestIdx := -1, -1
		for ai, am := range aMaps {
			if used[ai] {
				continue
			}
			s := blockMatchScore(bm, am, identityKeys)
			if s > bestScore {
				bestScore, bestIdx = s, ai
			}
		}

		bKey := blockKey(bm, identityKeys)
		if bestIdx >= 0 && bestScore > 0 {
			used[bestIdx] = true
			am := aMaps[bestIdx]
			aKey := blockKey(am, identityKeys)

			var unknownMap map[string]interface{}
			if entirelyUnknown {
				// Whole block is computed — all diffs are noise.
				unknownMap = allTrueMap(bm)
			} else if bestIdx < len(unknownSlice) {
				unknownMap, _ = unknownSlice[bestIdx].(map[string]interface{})
			}

			diffs := classifyBlockDiff(resType, blockType, bm, am, unknownMap, nreg)
			var semantic, noisy []AttrDiff
			for _, d := range diffs {
				if d.Noise {
					noisy = append(noisy, d)
				} else {
					semantic = append(semantic, d)
				}
			}
			if len(semantic) == 0 && len(noisy) > 0 {
				noise = append(noise, BlockNoise{BlockType: blockType, Key: bKey, Attrs: noisy})
			} else if len(semantic) > 0 {
				changed = append(changed, BlockChange{
					BlockType:     blockType,
					Key:           aKey,
					SemanticDiffs: semantic,
					NoiseDiffs:    noisy,
				})
			}
		} else {
			removed = append(removed, BlockSummary{
				BlockType: blockType,
				Key:       bKey,
				MainAttrs: summaryAttrs(bm, identityKeys),
			})
		}
	}
	for ai, am := range aMaps {
		if !used[ai] {
			aKey := blockKey(am, identityKeys)
			added = append(added, BlockSummary{
				BlockType: blockType,
				Key:       aKey,
				MainAttrs: summaryAttrs(am, identityKeys),
			})
		}
	}
	return
}

func classifyBlockDiff(
	resType, blockType string,
	before, after map[string]interface{},
	unknownMap map[string]interface{},
	nreg *normalsRegistry,
) []AttrDiff {
	seen := make(map[string]bool, len(before)+len(after))
	for k := range before {
		seen[k] = true
	}
	for k := range after {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var diffs []AttrDiff
	for _, k := range keys {
		bv := before[k]
		av := after[k]
		if jsonEqual(bv, av) {
			continue
		}
		computed := unknownBool(unknownMap, k)
		d := classifyAttr(resType, blockType, k, bv, av, computed, nreg)
		diffs = append(diffs, d)
	}
	return diffs
}

func classifyAttr(resType, blockType, attr string, before, after interface{}, computed bool, nreg *normalsRegistry) AttrDiff {
	d := AttrDiff{Attr: attr, Before: before, After: after}
	if computed {
		d.Noise = true
		d.Reason = "computed by provider"
		return d
	}
	// Attr absent in after (config dropped it) but before had a provider default.
	if after == nil && nreg.isKnownNormal(resType, blockType, attr, before) {
		d.Noise = true
		d.Reason = "optional attr absent in config (provider default)"
		return d
	}
	// Attr absent in before (wasn't in old state) but after has a provider default.
	if before == nil && nreg.isKnownNormal(resType, blockType, attr, after) {
		d.Noise = true
		d.Reason = "optional attr added with provider default value"
		return d
	}
	// Both values are in the normal (equivalent) set.
	if nreg.isBothNormal(resType, blockType, attr, before, after) {
		d.Noise = true
		d.Reason = "provider-normalized equivalent values"
		return d
	}
	return d
}

// ---- Helpers ---------------------------------------------------------------

func safeMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return map[string]interface{}{}
	}
	return m
}

func toSlice(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}

// unknownSliceFor extracts the per-element unknown info for blockType from
// after_unknown. Returns (nil, true) when the whole block attr is unknown.
func unknownSliceFor(afterUnknown map[string]interface{}, blockType string) (slice []interface{}, entirely bool) {
	v, ok := afterUnknown[blockType]
	if !ok {
		return nil, false
	}
	if b, ok := v.(bool); ok {
		return nil, b
	}
	s, _ := v.([]interface{})
	return s, false
}

func toStateMaps(slice []interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(slice))
	for _, v := range slice {
		if m, ok := v.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result
}

func blockKey(m map[string]interface{}, identityKeys []string) string {
	for _, k := range identityKeys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	// Fall back to "name" even when not the registered identity key.
	if n, ok := m["name"]; ok {
		if s, ok := n.(string); ok && s != "" {
			return s
		}
	}
	return "(unknown)"
}

func summaryAttrs(m map[string]interface{}, identityKeys []string) map[string]interface{} {
	out := make(map[string]interface{})
	for _, k := range identityKeys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	if _, ok := out["name"]; !ok {
		if v, ok := m["name"]; ok {
			out["name"] = v
		}
	}
	return out
}

func blockMatchScore(a, b map[string]interface{}, identityKeys []string) int {
	if len(identityKeys) > 0 {
		for _, k := range identityKeys {
			av, aok := a[k]
			bv, bok := b[k]
			if !aok || !bok {
				continue
			}
			if jsonEqual(av, bv) {
				return 100
			}
			return 0
		}
	}
	score := 0
	for k, av := range a {
		if bv, ok := b[k]; ok && jsonEqual(av, bv) {
			score++
		}
	}
	return score
}

func blockTypeSet(before, after map[string]interface{}) []string {
	seen := make(map[string]bool)
	for k, v := range before {
		if _, ok := v.([]interface{}); ok {
			seen[k] = true
		}
	}
	for k, v := range after {
		if _, ok := v.([]interface{}); ok {
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func unknownBool(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// allTrueMap returns a map where every key from src maps to true (all computed).
func allTrueMap(src map[string]interface{}) map[string]interface{} {
	m := make(map[string]interface{}, len(src))
	for k := range src {
		m[k] = true
	}
	return m
}

func jsonEqual(a, b interface{}) bool {
	if a == nil && b == nil {
		return true
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}
