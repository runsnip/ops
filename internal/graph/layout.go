package graph

import (
	"sort"
)

// layout places nodes in columns, left to right: ingress (hostnames), edge (proxies, in the order requests cross
// them), app (services, deeper when only other services reach them), data, external (outside hosts, registries).
// Within a column nodes are ordered by the mean row of what points at them (one barycentre sweep), then by name:
// the same stack always gives the same picture.
func (b *builder) layout() {
	band := map[string]int{"ingress": 0, "edge": 1, "app": 2, "data": 3, "external": 4}
	for _, id := range b.order {
		n := b.nodes[id]
		if n.Layer != "" {
			if _, ok := band[n.Layer]; ok {
				continue
			}
		}
		switch n.Kind {
		case KindHost:
			n.Layer = "ingress"
		case KindProxy:
			n.Layer = "edge"
		case KindData:
			n.Layer = "data"
		case KindExternal, KindRegistry:
			n.Layer = "external"
		default:
			n.Layer = "app"
		}
	}
	// Depth inside the edge and app bands: the longest chain of route/calls edges within the band (cycles cut).
	out := map[string][]string{}
	for _, k := range b.eord {
		e := b.edges[k]
		if e.Kind == EdgeRoute || e.Kind == EdgeCalls {
			out[e.From] = append(out[e.From], e.To)
		}
	}
	depth := map[string]int{}
	for _, layer := range []string{"edge", "app"} {
		var walk func(id string, d int, seen map[string]bool)
		walk = func(id string, d int, seen map[string]bool) {
			if seen[id] || d > 3 {
				return
			}
			if d > depth[id] {
				depth[id] = d
			}
			seen[id] = true
			for _, to := range out[id] {
				if n := b.nodes[to]; n != nil && n.Layer == layer {
					walk(to, d+1, seen)
				}
			}
			delete(seen, id)
		}
		for _, id := range b.order {
			if b.nodes[id].Layer == layer {
				walk(id, 0, map[string]bool{})
			}
		}
	}
	// Columns: each band takes as many columns as its deepest chain.
	width := map[string]int{}
	for _, id := range b.order {
		n := b.nodes[id]
		if depth[id]+1 > width[n.Layer] {
			width[n.Layer] = depth[id] + 1
		}
	}
	start, col := map[string]int{}, 0
	for _, l := range Layers {
		start[l] = col
		if width[l] > 0 {
			col += width[l]
		}
	}
	cols := map[int][]string{}
	for _, id := range b.order {
		n := b.nodes[id]
		n.Col = start[n.Layer] + depth[id]
		cols[n.Col] = append(cols[n.Col], id)
	}
	in := map[string][]string{}
	for _, k := range b.eord {
		e := b.edges[k]
		in[e.To] = append(in[e.To], e.From)
	}
	var cs []int
	for c := range cols {
		cs = append(cs, c)
	}
	sort.Ints(cs)
	for _, c := range cs {
		ids := cols[c]
		bary := map[string]float64{}
		for _, id := range ids {
			sum, n := 0.0, 0
			for _, f := range in[id] {
				if p := b.nodes[f]; p != nil && p.Col < c {
					sum += float64(p.Row)
					n++
				}
			}
			bary[id] = 1e9 // nothing points at it from the left: after those that something does
			if n > 0 {
				bary[id] = sum / float64(n)
			}
		}
		sort.SliceStable(ids, func(i, j int) bool {
			a, z := b.nodes[ids[i]], b.nodes[ids[j]]
			if bary[ids[i]] != bary[ids[j]] {
				return bary[ids[i]] < bary[ids[j]]
			}
			if a.Group != z.Group {
				return a.Group < z.Group
			}
			return a.Name < z.Name
		})
		for r, id := range ids {
			b.nodes[id].Row = r
		}
	}
}
