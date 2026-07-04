package stdlibreg

import "strings"

func init() {
	Register(&Package{Name: "strings", Symbols: map[string]any{
		"Contains":    strings.Contains,
		"ContainsAny": strings.ContainsAny,
		"Count":       strings.Count,
		"EqualFold":   strings.EqualFold,
		"Fields":      strings.Fields,
		"HasPrefix":   strings.HasPrefix,
		"HasSuffix":   strings.HasSuffix,
		"Index":       strings.Index,
		"Join":        strings.Join,
		"LastIndex":   strings.LastIndex,
		"Repeat":      strings.Repeat,
		"Replace":     strings.Replace,
		"ReplaceAll":  strings.ReplaceAll,
		"Split":       strings.Split,
		"SplitN":      strings.SplitN,
		"Title":       strings.ToTitle,
		"ToLower":     strings.ToLower,
		"ToUpper":     strings.ToUpper,
		"Trim":        strings.Trim,
		"TrimLeft":    strings.TrimLeft,
		"TrimPrefix":  strings.TrimPrefix,
		"TrimRight":   strings.TrimRight,
		"TrimSpace":   strings.TrimSpace,
		"TrimSuffix":  strings.TrimSuffix,
		"CutPrefix":   strings.CutPrefix,
		"CutSuffix":   strings.CutSuffix,
		"Cut":         strings.Cut,
	}})
}
