package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
)

type SerializableVar struct {
	Type     string
	V        string
	Origin   string
	Children []SerializableVar
}

type SerializableDepNode struct {
	Output             int
	Cmds               []string
	Deps               []int
	HasRule            bool
	IsOrderOnly        bool
	IsPhony            bool
	ActualInputs       []int
	TargetSpecificVars []int
	Filename           string
	Lineno             int
}

type SerializableTargetSpecificVar struct {
	Name  string
	Value SerializableVar
}

type SerializableGraph struct {
	Nodes   []*SerializableDepNode
	Vars    map[string]SerializableVar
	Tsvs    []SerializableTargetSpecificVar
	Targets []string
}

func encGob(v interface{}) string {
	var buf bytes.Buffer
	e := gob.NewEncoder(&buf)
	err := e.Encode(v)
	if err != nil {
		panic(err)
	}
	return buf.String()
}

type DepNodesSerializer struct {
	nodes     []*SerializableDepNode
	tsvs      []SerializableTargetSpecificVar
	tsvMap    map[string]int
	targets   []string
	targetMap map[string]int
	done      map[string]bool
}

func NewDepNodesSerializer() *DepNodesSerializer {
	return &DepNodesSerializer{
		tsvMap:    make(map[string]int),
		targetMap: make(map[string]int),
		done:      make(map[string]bool),
	}
}

func (ns *DepNodesSerializer) SerializeTarget(t string) int {
	id, present := ns.targetMap[t]
	if present {
		return id
	}
	id = len(ns.targets)
	ns.targetMap[t] = id
	ns.targets = append(ns.targets, t)
	return id
}

func (ns *DepNodesSerializer) SerializeDepNodes(nodes []*DepNode) {
	for _, n := range nodes {
		if ns.done[n.Output] {
			continue
		}
		ns.done[n.Output] = true

		var deps []int
		for _, d := range n.Deps {
			deps = append(deps, ns.SerializeTarget(d.Output))
		}
		var actualInputs []int
		for _, i := range n.ActualInputs {
			actualInputs = append(actualInputs, ns.SerializeTarget(i))
		}

		// Sort keys for consistent serialization.
		var tsvKeys []string
		for k := range n.TargetSpecificVars {
			tsvKeys = append(tsvKeys, k)
		}
		sort.Strings(tsvKeys)

		var vars []int
		for _, k := range tsvKeys {
			v := n.TargetSpecificVars[k]
			sv := SerializableTargetSpecificVar{Name: k, Value: v.Serialize()}
			gob := encGob(sv)
			id, present := ns.tsvMap[gob]
			if !present {
				id = len(ns.tsvs)
				ns.tsvMap[gob] = id
				ns.tsvs = append(ns.tsvs, sv)
			}
			vars = append(vars, id)
		}

		ns.nodes = append(ns.nodes, &SerializableDepNode{
			Output:             ns.SerializeTarget(n.Output),
			Cmds:               n.Cmds,
			Deps:               deps,
			HasRule:            n.HasRule,
			IsOrderOnly:        n.IsOrderOnly,
			IsPhony:            n.IsPhony,
			ActualInputs:       actualInputs,
			TargetSpecificVars: vars,
			Filename:           n.Filename,
			Lineno:             n.Lineno,
		})
		ns.SerializeDepNodes(n.Deps)
	}
}

func MakeSerializableVars(vars Vars) (r map[string]SerializableVar) {
	r = make(map[string]SerializableVar)
	for k, v := range vars {
		r[k] = v.Serialize()
	}
	return r
}

func MakeSerializableGraph(nodes []*DepNode, vars Vars) SerializableGraph {
	ns := NewDepNodesSerializer()
	ns.SerializeDepNodes(nodes)
	v := MakeSerializableVars(vars)
	return SerializableGraph{
		Nodes: ns.nodes,
		Vars: v,
		Tsvs: ns.tsvs,
		Targets: ns.targets,
	}
}

func DumpDepGraphAsJson(nodes []*DepNode, vars Vars, filename string) {
	o, err := json.MarshalIndent(MakeSerializableGraph(nodes, vars), " ", " ")
	if err != nil {
		panic(err)
	}
	f, err2 := os.Create(filename)
	if err2 != nil {
		panic(err2)
	}
	f.Write(o)
}

func DumpDepGraph(nodes []*DepNode, vars Vars, filename string) {
	f, err := os.Create(filename)
	if err != nil {
		panic(err)
	}
	e := gob.NewEncoder(f)
	e.Encode(MakeSerializableGraph(nodes, vars))
}

func DeserializeSingleChild(sv SerializableVar) Value {
	if len(sv.Children) != 1 {
		panic(fmt.Sprintf("unexpected number of children: %q", sv))
	}
	return DeserializeVar(sv.Children[0])
}

func DeserializeVar(sv SerializableVar) (r Value) {
	switch sv.Type {
	case "literal":
		return literal(sv.V)
	case "tmpval":
		return tmpval([]byte(sv.V))
	case "expr":
		var e Expr
		for _, v := range sv.Children {
			e = append(e, DeserializeVar(v))
		}
		return e
	case "varref":
		return varref{varname: DeserializeSingleChild(sv)}
	case "paramref":
		v, err := strconv.Atoi(sv.V)
		if err != nil {
			panic(err)
		}
		return paramref(v)
	case "varsubst":
		return varsubst{
			varname: DeserializeVar(sv.Children[0]),
			pat:     DeserializeVar(sv.Children[1]),
			subst:   DeserializeVar(sv.Children[2]),
		}

	case "func":
		name := DeserializeVar(sv.Children[0]).(literal)
		f := funcMap[string(name[1:])]()
		f.AddArg(name)
		for _, a := range sv.Children[1:] {
			f.AddArg(DeserializeVar(a))
		}
		return f
	case "funcEvalAssign":
		return &funcEvalAssign{
			lhs: sv.Children[0].V,
			op:  sv.Children[1].V,
			rhs: DeserializeVar(sv.Children[2]),
		}
	case "funcNop":
		return &funcNop{expr: sv.V}

	case "simple":
		return SimpleVar{
			value:  []byte(sv.V),
			origin: sv.Origin,
		}
	case "recursive":
		return RecursiveVar{
			expr:   DeserializeSingleChild(sv),
			origin: sv.Origin,
		}

	case ":=", "=", "+=", "?=":
		return TargetSpecificVar{
			v:  DeserializeSingleChild(sv).(Var),
			op: sv.Type,
		}

	default:
		panic(fmt.Sprintf("unknown serialized variable type: %q", sv))
	}
	return UndefinedVar{}
}

func DeserializeVars(vars map[string]SerializableVar) Vars {
	r := make(Vars)
	for k, v := range vars {
		r[k] = DeserializeVar(v).(Var)
	}
	return r
}

func DeserializeNodes(g SerializableGraph) (r []*DepNode) {
	nodes := g.Nodes
	tsvs := g.Tsvs
	targets := g.Targets
	// Deserialize all TSVs first so that multiple rules can share memory.
	var tsvValues []Var
	for _, sv := range tsvs {
		tsvValues = append(tsvValues, DeserializeVar(sv.Value).(Var))
	}

	nodeMap := make(map[string]*DepNode)
	for _, n := range nodes {
		var actualInputs []string
		for _, i := range n.ActualInputs {
			actualInputs = append(actualInputs, targets[i])
		}

		d := &DepNode{
			Output:             targets[n.Output],
			Cmds:               n.Cmds,
			HasRule:            n.HasRule,
			IsOrderOnly:        n.IsOrderOnly,
			IsPhony:            n.IsPhony,
			ActualInputs:       actualInputs,
			Filename:           n.Filename,
			Lineno:             n.Lineno,
			TargetSpecificVars: make(Vars),
		}

		for _, id := range n.TargetSpecificVars {
			sv := tsvs[id]
			d.TargetSpecificVars[sv.Name] = tsvValues[id]
		}

		nodeMap[targets[n.Output]] = d
		r = append(r, d)
	}

	for _, n := range nodes {
		d := nodeMap[targets[n.Output]]
		for _, o := range n.Deps {
			c, present := nodeMap[targets[o]]
			if !present {
				panic(fmt.Sprintf("unknown target: %s", o))
			}
			d.Deps = append(d.Deps, c)
		}
	}

	return r
}

func human(n int) string {
	if n >= 10 * 1000 * 1000 * 1000 {
		return fmt.Sprintf("%.2fGB", float32(n) / 1000 / 1000 / 1000)
	} else if n >= 10 * 1000 * 1000 {
		return fmt.Sprintf("%.2fMB", float32(n) / 1000 / 1000)
	} else if n >= 10 * 1000 {
		return fmt.Sprintf("%.2fkB", float32(n) / 1000)
	} else {
		return fmt.Sprintf("%dB", n)
	}
}

func showSerializedNodesStats(nodes []*SerializableDepNode) {
	outputSize := 0
	cmdSize := 0
	depsSize := 0
	actualInputSize := 0
	tsvSize := 0
	filenameSize := 0
	linenoSize := 0
	for _, n := range nodes {
		outputSize += 4
		for _, c := range n.Cmds {
			cmdSize += len(c)
		}
		for _ = range n.Deps {
			depsSize += 4
		}
		for _ = range n.ActualInputs {
			actualInputSize += 4
		}
		for _ = range n.TargetSpecificVars {
			tsvSize += 4
		}
		filenameSize += len(n.Filename)
		linenoSize += 4
	}
	size := outputSize + cmdSize + depsSize + actualInputSize + tsvSize + filenameSize + linenoSize
	LogStats("%d nodes %s", len(nodes), human(size))
	LogStats(" output %s", human(outputSize))
	LogStats(" command %s", human(cmdSize))
	LogStats(" deps %s", human(depsSize))
	LogStats(" inputs %s", human(actualInputSize))
	LogStats(" tsv %s", human(tsvSize))
	LogStats(" filename %s", human(filenameSize))
	LogStats(" lineno %s", human(linenoSize))
}

func (v SerializableVar) size() int {
	size := 0
	size += len(v.Type)
	size += len(v.V)
	size += len(v.Origin)
	for _, c := range v.Children {
		size += c.size()
	}
	return size
}

func showSerializedVarsStats(vars map[string]SerializableVar) {
	nameSize := 0
	valueSize := 0
	for k, v := range vars {
		nameSize += len(k)
		valueSize += v.size()
	}
	size := nameSize + valueSize
	LogStats("%d vars %s", len(vars), human(size))
	LogStats(" name %s", human(nameSize))
	LogStats(" value %s", human(valueSize))
}

func showSerializedTsvsStats(vars []SerializableTargetSpecificVar) {
	nameSize := 0
	valueSize := 0
	for _, v := range vars {
		nameSize += len(v.Name)
		valueSize += v.Value.size()
	}
	size := nameSize + valueSize
	LogStats("%d tsvs %s", len(vars), human(size))
	LogStats(" name %s", human(nameSize))
	LogStats(" value %s", human(valueSize))
}

func showSerializedTargetsStats(targets []string) {
	size := 0
	for _, t := range targets {
		size += len(t)
	}
	LogStats("%d targets %s", len(targets), human(size))
}

func showSerializedGraphStats(g SerializableGraph) {
	showSerializedNodesStats(g.Nodes)
	showSerializedVarsStats(g.Vars)
	showSerializedTsvsStats(g.Tsvs)
	showSerializedTargetsStats(g.Targets)
}

func DeserializeGraph(g SerializableGraph) ([]*DepNode, Vars) {
	if katiLogFlag || katiStatsFlag {
		showSerializedGraphStats(g)
	}
	nodes := DeserializeNodes(g)
	vars := DeserializeVars(g.Vars)
	return nodes, vars
}

func LoadDepGraphFromJson(filename string) ([]*DepNode, Vars) {
	f, err := os.Open(filename)
	if err != nil {
		panic(err)
	}

	d := json.NewDecoder(f)
	g := SerializableGraph{Vars: make(map[string]SerializableVar)}
	err = d.Decode(&g)
	if err != nil {
		panic(err)
	}
	return DeserializeGraph(g)
}

func LoadDepGraph(filename string) ([]*DepNode, Vars) {
	f, err := os.Open(filename)
	if err != nil {
		panic(err)
	}

	d := gob.NewDecoder(f)
	g := SerializableGraph{Vars: make(map[string]SerializableVar)}
	err = d.Decode(&g)
	if err != nil {
		panic(err)
	}
	return DeserializeGraph(g)
}