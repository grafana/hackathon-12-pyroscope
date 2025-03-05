package symdb

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/pyroscope/pkg/pprof"
)

func Test_stacktrace_tree_encoding(t *testing.T) {
	stacks := [][]uint64{
		{5, 4, 3, 2, 1},
		{6, 4, 3, 2, 1},
		{4, 3, 2, 1},
		{3, 2, 1},
		{4, 2, 1},
		{7, 2, 1},
		{2, 1},
		{1},
	}

	x := newStacktraceTree(10)
	var b bytes.Buffer

	for i := range stacks {
		x.insert(stacks[i])

		b.Reset()
		_, err := x.WriteTo(&b)
		require.NoError(t, err)

		ppt := newParentPointerTree(x.len())
		_, err = ppt.ReadFrom(bytes.NewBuffer(b.Bytes()))
		require.NoError(t, err)

		for j := range x.nodes {
			n, p := x.nodes[j], ppt.nodes[j]
			if n.p != p.p || n.r != p.r {
				t.Fatalf("tree mismatch on %v: n:%#v, p:%#v", stacks[i], n, p)
			}
		}
	}
}

func Test_stacktrace_tree_encoding_group(t *testing.T) {
	stacks := [][]uint64{
		{5, 4, 3, 2, 1},
		{6, 4, 3, 2, 1},
		{4, 3, 2, 1},
		{3, 2, 1},
		{4, 2, 1},
		{7, 2, 1},
		{2, 1},
		{1},
	}

	x := newStacktraceTree(10)
	var b bytes.Buffer

	for i := range stacks {
		x.insert(stacks[i])

		b.Reset()
		e := treeEncoder{writeSize: 30}
		err := e.marshal(x, &b)
		require.NoError(t, err)

		ppt := newParentPointerTree(x.len())
		d := treeDecoder{
			bufSize:     64,
			peekSize:    20,
			groupBuffer: 12,
		}
		err = d.unmarshal(ppt, bytes.NewBuffer(b.Bytes()))
		require.NoError(t, err)

		for j := range x.nodes {
			n, p := x.nodes[j], ppt.nodes[j]
			if n.p != p.p || n.r != p.r {
				t.Fatalf("tree mismatch on %v: n:%#v, p:%#v", stacks[i], n, p)
			}
		}
	}
}

func Test_stacktrace_tree_encoding_rand(t *testing.T) {
	nodes := make([]node, 1<<20)
	for i := range nodes {
		nodes[i] = node{
			fc: 2,
			ns: 3,
			p:  int32(rand.Intn(10 << 10)),
			r:  int32(rand.Intn(10 << 10)),
		}
	}

	x := &stacktraceTree{nodes: nodes}
	var b bytes.Buffer
	_, err := x.WriteTo(&b)
	require.NoError(t, err)

	ppt := newParentPointerTree(x.len())
	_, err = ppt.ReadFrom(bytes.NewBuffer(b.Bytes()))
	require.NoError(t, err)

	for j := range x.nodes {
		n, p := x.nodes[j], ppt.nodes[j]
		if n.p != p.p || n.r != p.r {
			t.Fatalf("tree mismatch at %d: n:%#v. p:%#v", j, n, p)
		}
	}
}

func Test_stacktrace_tree_pprof_locations_(t *testing.T) {
	x := newStacktraceTree(0)
	assert.Len(t, x.resolve([]int32{0, 1, 2, 3}, 42), 0)
	assert.Len(t, x.resolveUint64([]uint64{0, 1, 2, 3}, 42), 0)

	p := newParentPointerTree(0)
	assert.Len(t, p.resolve([]int32{0, 1, 2, 3}, 42), 0)
	assert.Len(t, p.resolveUint64([]uint64{0, 1, 2, 3}, 42), 0)
}

func Test_stacktrace_tree_pprof_locations(t *testing.T) {
	p, err := pprof.OpenFile("testdata/profile.pb.gz")
	require.NoError(t, err)

	x := newStacktraceTree(defaultStacktraceTreeSize)
	m := make(map[uint32]int)
	for i := range p.Sample {
		m[x.insert(p.Sample[i].LocationId)] = i
	}

	tmp := stacktraceLocations.get()
	defer stacktraceLocations.put(tmp)
	for sid, i := range m {
		tmp = x.resolve(tmp, sid)
		locs := p.Sample[i].LocationId
		for j := range locs {
			if tmp[j] != int32(locs[j]) {
				t.Log("resolved:", tmp)
				t.Log("locations:", locs)
				t.Fatalf("ST: tmp[j] != locs[j]")
			}
		}
	}

	var b bytes.Buffer
	n, err := x.WriteTo(&b)
	require.NoError(t, err)
	assert.Equal(t, b.Len(), int(n))

	ppt := newParentPointerTree(x.len())
	n, err = ppt.ReadFrom(bytes.NewReader(b.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, b.Len(), int(n))

	tmp = stacktraceLocations.get()
	defer stacktraceLocations.put(tmp)
	for sid, i := range m {
		tmp = ppt.resolve(tmp, sid)
		locs := p.Sample[i].LocationId
		for j := range locs {
			if tmp[j] != int32(locs[j]) {
				t.Log("resolved:", tmp)
				t.Log("locations:", locs)
				t.Fatalf("PPT: tmp[j] != locs[j]")
			}
		}
	}
}

// The test is helpful for debugging.
func Test_parentPointerTree_toStacktraceTree(t *testing.T) {
	x := newStacktraceTree(10)
	for _, stack := range [][]uint64{
		{5, 4, 3, 2, 1},
		{6, 4, 3, 2, 1},
		{4, 3, 2, 1},
		{3, 2, 1},
		{4, 2, 1},
		{7, 2, 1},
		{2, 1},
		{1},
	} {
		x.insert(stack)
	}
	assertRestoredStacktraceTree(t, x)
}

func Test_parentPointerTree_toStacktraceTree_profile(t *testing.T) {
	p, err := pprof.OpenFile("testdata/profile.pb.gz")
	require.NoError(t, err)
	x := newStacktraceTree(defaultStacktraceTreeSize)
	for _, s := range p.Sample {
		x.insert(s.LocationId)
	}
	assertRestoredStacktraceTree(t, x)
}

func assertRestoredStacktraceTree(t *testing.T, x *stacktraceTree) {
	var b bytes.Buffer
	_, _ = x.WriteTo(&b)
	ppt := newParentPointerTree(x.len())
	_, err := ppt.ReadFrom(bytes.NewBuffer(b.Bytes()))
	require.NoError(t, err)
	restored := ppt.toStacktraceTree()
	assert.Equal(t, x.nodes, restored.nodes)
}

func Benchmark_stacktrace_tree_insert(b *testing.B) {
	p, err := pprof.OpenFile("testdata/profile.pb.gz")
	require.NoError(b, err)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		x := newStacktraceTree(0)
		for j := range p.Sample {
			x.insert(p.Sample[j].LocationId)
		}
	}
}

func Test_stacktrace_tree_insert(t *testing.T) {
	// Create a new tree
	tree := newStacktraceTree(0)

	// Test simple stack insertion
	stack1 := []uint64{3, 2, 1} // Bottom-up stack: main->func2->func3
	id1 := tree.insert(stack1)

	// Verify the structure
	require.Equal(t, uint32(3), id1)     // Should point to the leaf node (3)
	require.Equal(t, 4, len(tree.nodes)) // Root + 3 nodes

	// Root node (0)
	require.Equal(t, int32(-1), tree.nodes[0].p)
	require.Equal(t, int32(1), tree.nodes[0].fc)  // Points to node 1
	require.Equal(t, int32(-1), tree.nodes[0].ns) // No siblings
	require.Equal(t, int32(0), tree.nodes[0].r)   // No reference

	// Node 1 (represents 1 in the stack)
	require.Equal(t, int32(0), tree.nodes[1].p)   // Parent is root
	require.Equal(t, int32(1), tree.nodes[1].r)   // References location 1
	require.Equal(t, int32(2), tree.nodes[1].fc)  // Points to node 2
	require.Equal(t, int32(-1), tree.nodes[1].ns) // No siblings

	// Node 2 (represents 2 in the stack)
	require.Equal(t, int32(1), tree.nodes[2].p)   // Parent is node 1
	require.Equal(t, int32(2), tree.nodes[2].r)   // References location 2
	require.Equal(t, int32(3), tree.nodes[2].fc)  // Points to node 3
	require.Equal(t, int32(-1), tree.nodes[2].ns) // No siblings

	// Node 3 (represents 3 in the stack)
	require.Equal(t, int32(2), tree.nodes[3].p)   // Parent is node 2
	require.Equal(t, int32(3), tree.nodes[3].r)   // References location 3
	require.Equal(t, int32(-1), tree.nodes[3].fc) // No children
	require.Equal(t, int32(-1), tree.nodes[3].ns) // No siblings

	// Insert a stack sharing some nodes
	stack2 := []uint64{4, 2, 1} // Bottom-up stack: main->func2->func4
	id2 := tree.insert(stack2)

	// Verify the new structure
	require.Equal(t, uint32(4), id2)     // Should point to the new leaf node (4)
	require.Equal(t, 5, len(tree.nodes)) // One new node added

	// Node 4 (represents 4 in the stack)
	require.Equal(t, int32(2), tree.nodes[4].p)   // Parent is node 2
	require.Equal(t, int32(4), tree.nodes[4].r)   // References location 4
	require.Equal(t, int32(-1), tree.nodes[4].fc) // No children
	require.Equal(t, int32(-1), tree.nodes[4].ns) // No siblings

	// Node 3 should now have node 4 as a sibling
	require.Equal(t, int32(4), tree.nodes[3].ns)

	// Now test with real profile data
	p, err := pprof.OpenFile("testdata/profile.pb.gz")
	require.NoError(t, err)

	tree = newStacktraceTree(0)
	var insertedIds []uint32

	// Insert all stacks from the profile
	for _, sample := range p.Sample {
		id := tree.insert(sample.LocationId)
		insertedIds = append(insertedIds, id)
	}

	// Verify each inserted stack can be resolved
	tmp := make([]int32, 0)
	for i, id := range insertedIds {
		resolved := tree.resolve(tmp, id)
		// Verify the resolved stack matches the original
		for j, loc := range resolved {
			require.Equal(t, int32(p.Sample[i].LocationId[j]), loc)
		}
	}
}
