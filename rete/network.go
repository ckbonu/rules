package rete

import (
	"context"
	"fmt"
	"math"

	"github.com/TIBCOSoftware/bego/common/model"

	"container/list"
	"sync"
)

//Network ... the rete network
type Network interface {
	AddRule(model.Rule) int
	String() string
	RemoveRule(string) model.Rule
	Assert(ctx context.Context, rs model.RuleSession, tuple model.StreamTuple)
	Retract(ctx context.Context, tuple model.StreamTuple)

	assertInternal(ctx context.Context, tuple model.StreamTuple)
	getOrCreateHandle(tuple model.StreamTuple) reteHandle
	incrementAndGetId() int
}

type reteNetworkImpl struct {
	//All rules in the network
	allRules map[string]model.Rule //(Rule)

	//Holds the DataSource name as key, and ClassNodes as value
	allClassNodes map[string]classNode //ClassNode in network

	//Holds the Rule name as key and pointer to a slice of RuleNodes as value
	ruleNameNodesOfRule map[string]*list.List //*list.List of Nodes of rule

	//Holds the Rule name as key and a pointer to a slice of NodeLinks as value
	ruleNameClassNodeLinksOfRule map[string]*list.List //*list.List of ClassNodeLink

	allHandles map[model.StreamTuple]reteHandle

	currentId int

	assertLock sync.Mutex
	crudLock sync.Mutex
}

//NewReteNetwork ... creates a new rete network
func NewReteNetwork() Network {
	reteNetworkImpl := reteNetworkImpl{}
	reteNetworkImpl.initReteNetwork()
	return &reteNetworkImpl
}

func (nw *reteNetworkImpl) initReteNetwork() {
	nw.allRules = make(map[string]model.Rule)
	nw.allClassNodes = make(map[string]classNode)
	nw.ruleNameNodesOfRule = make(map[string]*list.List)
	nw.ruleNameClassNodeLinksOfRule = make(map[string]*list.List)
	nw.allHandles = make(map[model.StreamTuple]reteHandle)
}

func (nw *reteNetworkImpl) AddRule(rule model.Rule) int {

	nw.crudLock.Lock()
	defer nw.crudLock.Unlock()

	if nw.allRules[rule.GetName()] != nil {
		fmt.Println("Rule already exists.." + rule.GetName())
		return 1
	}
	//TODO: Worry about nonEqJoin warnings later.
	conditionSet := list.New()
	conditionSetNoIdr := list.New()
	nodeSet := list.New()

	nodesOfRule := list.New()
	classNodeLinksOfRule := list.New()

	if len(rule.GetConditions()) == 0 {
		identifierVar := pickIdentifier(rule.GetIdentifiers())
		nw.createClassFilterNode(rule, nodesOfRule, classNodeLinksOfRule, identifierVar, nil, nodeSet)
	} else {
		for i := 0; i < len(rule.GetConditions()); i++ {
			if rule.GetConditions()[i].GetIdentifiers() == nil || len(rule.GetConditions()[i].GetIdentifiers()) == 0 {
				//TODO: condition with no identifiers
				conditionSetNoIdr.PushBack(rule.GetConditions()[i])
			} else if len(rule.GetConditions()[i].GetIdentifiers()) == 1 &&
				!contains(nodeSet, rule.GetConditions()[i].GetIdentifiers()[0]) {
				cond := rule.GetConditions()[i]
				nw.createClassFilterNode(rule, nodesOfRule, classNodeLinksOfRule, cond.GetIdentifiers()[0], cond, nodeSet)
			} else {
				conditionSet.PushBack(rule.GetConditions()[i])
			}
		}
	}

	nw.buildNetwork(rule, nodesOfRule, classNodeLinksOfRule, conditionSet, nodeSet, conditionSetNoIdr)

	cntxt := make([]interface{}, 2)
	cntxt[0] = nw
	cntxt[1] = nodesOfRule

	for _, classNode := range nw.allClassNodes {
		optimizeNetwork(classNode, cntxt)
	}
	// nw.optimizeNetwork(nodesOfRule)

	nw.setClassNodeAndLinkJoinTables(nodesOfRule, classNodeLinksOfRule)

	//Add the rule to the network
	nw.allRules[rule.GetName()] = rule

	//Add RuleNodes
	nw.ruleNameNodesOfRule[rule.GetName()] = nodesOfRule

	//Add NodeLinks
	nw.ruleNameClassNodeLinksOfRule[rule.GetName()] = classNodeLinksOfRule
	return 0
}

func (nw *reteNetworkImpl) setClassNodeAndLinkJoinTables(nodesOfRule *list.List,
	classNodeLinksOfRule *list.List) {
	//TODO: add join table ids to nodes and links
}

func (nw *reteNetworkImpl) RemoveRule(ruleName string) model.Rule {

	nw.crudLock.Lock()
	defer nw.crudLock.Unlock()

	rule := nw.allRules[ruleName]
	delete(nw.allRules, ruleName)
	if rule == nil {
		//TODO: log a message
		return nil
	}

	classNodeLinksOfRule := nw.ruleNameClassNodeLinksOfRule[ruleName]
	delete(nw.ruleNameClassNodeLinksOfRule, ruleName)
	if classNodeLinksOfRule != nil {

		for e := classNodeLinksOfRule.Front(); e != nil; e = e.Next() {
			removeRuleHelper(e.Value.(classNodeLink))
		}

	}

	nodesOfRuleItem := nw.ruleNameNodesOfRule[ruleName]
	delete(nw.ruleNameNodesOfRule, ruleName)
	if nodesOfRuleItem != nil {
		for e := nodesOfRuleItem.Front(); e != nil; e = e.Next() {
			node := e.Value.(abstractNode)
			switch nodeImpl := node.(type) {
			//Only interested in joinnodes
			//case *filterNodeImpl:
			//case *classNodeImpl:
			//case *ruleNodeImpl:
			case *joinNodeImpl:
				removeRefsFromReteHandles(nodeImpl.leftTable)
				removeRefsFromReteHandles(nodeImpl.rightTable)
			}
		}
	}

	return rule
}

func removeRefsFromReteHandles(joinTableVar joinTable) {
	if joinTableVar == nil {
		return
	}
	for tableRow := range joinTableVar.getMap() {
		for _, handle := range tableRow.getHandles() {
			handle.removeJoinTable(joinTableVar)
		}
	}
}

func removeRuleHelper(classNodeLinkOfRule classNodeLink) {
	classNodeLinkOfRule.getClassNode().removeClassNodeLink(classNodeLinkOfRule)
}

func optimizeNetwork(classNodeVar classNode, context []interface{}) {
	nodesOfRule := context[1].(*list.List)
	for e := classNodeVar.getClassNodeLinks().Front(); e != nil; e = e.Next() {
		nodeLink := e.Value.(classNodeLink)
		childNode := nodeLink.getChild()
		switch nodeImpl := childNode.(type) {
		case *filterNodeImpl:
			if nodeImpl.conditionVar == nil {
				nodeLink.setChild(nodeImpl.nodeLinkVar.getChild())
				nodeLink.setIsRightChild(nodeImpl.nodeLinkVar.isRightNode())
				removeFromList(nodesOfRule, nodeImpl)
			}
		}
	}
}

func removeFromList(listVar *list.List, val interface{}) {
	for e := listVar.Front(); e != nil; e = e.Next() {
		if e.Value == val {
			listVar.Remove(e)
			break
		}
	}
}

func contains(nodeSet *list.List, identifierVar model.TupleTypeAlias) bool {
	identifiers := []model.TupleTypeAlias{identifierVar}
	for e := nodeSet.Front(); e != nil; e = e.Next() {
		node := e.Value.(node)
		if ContainedByFirst(node.getIdentifiers(), identifiers) {
			return true
		}
	}
	return false
}

func (nw *reteNetworkImpl) buildNetwork(rule model.Rule, nodesOfRule *list.List, classNodeLinksOfRule *list.List,
	conditionSet *list.List, nodeSet *list.List, conditionSetNoIdr *list.List) {
	if conditionSet.Len() == 0 {
		if nodeSet.Len() == 1 {
			node := nodeSet.Front().Value.(node)
			if ContainedByFirst(node.getIdentifiers(), rule.GetIdentifiers()) {
				//TODO: Re evaluate set later..

				lastNode := node
				//check conditions with no identifierVar
				for e := conditionSetNoIdr.Front(); e != nil; e = e.Next() {
					conditionVar := e.Value.(model.Condition)
					fNode := newFilterNode(nw, node.getIdentifiers(), conditionVar)
					nodesOfRule.PushBack(fNode)
					newNodeLink(nw, lastNode, fNode, false)
					lastNode = fNode
				}
				//Yoohoo! We have a Rule!!
				ruleNode := newRuleNode(rule)
				newNodeLink(nw, node, ruleNode, false)
				nodesOfRule.PushBack(ruleNode)
			} else {
				idrs := SecondMinusFirst(node.getIdentifiers(), rule.GetIdentifiers())
				fNode := nw.createClassFilterNode(rule, nodesOfRule, classNodeLinksOfRule, idrs[0], nil, nodeSet)
				nw.createJoinNode(rule, nodesOfRule, node, fNode, nil, conditionSet, nodeSet)
				nw.buildNetwork(rule, nodesOfRule, classNodeLinksOfRule, conditionSet, nodeSet, conditionSetNoIdr)
			}
		} else {
			nodes := findSimilarNodes(nodeSet)
			nw.createJoinNode(rule, nodesOfRule, nodes[0], nodes[1], nil, conditionSet, nodeSet)
			nw.buildNetwork(rule, nodesOfRule, classNodeLinksOfRule, conditionSet, nodeSet, conditionSetNoIdr)
		}
	} else {
		if nw.createFilterNode(rule, nodesOfRule, conditionSet, nodeSet) {
			nw.buildNetwork(rule, nodesOfRule, classNodeLinksOfRule, conditionSet, nodeSet, conditionSetNoIdr)
		} else if nw.createJoinNodeFromExisting(rule, nodesOfRule, conditionSet, nodeSet) {
			nw.buildNetwork(rule, nodesOfRule, classNodeLinksOfRule, conditionSet, nodeSet, conditionSetNoIdr)
		} else if nw.createJoinNodeFromSome(rule, nodesOfRule, classNodeLinksOfRule, conditionSet, nodeSet) {
			nw.buildNetwork(rule, nodesOfRule, classNodeLinksOfRule, conditionSet, nodeSet, conditionSetNoIdr)
		} else {
			conditionVar := nw.findConditionWithLeastIdentifiers(conditionSet)
			nw.createClassFilterNode(rule, nodesOfRule, classNodeLinksOfRule, conditionVar.GetIdentifiers()[0], nil, nodeSet)
			nw.buildNetwork(rule, nodesOfRule, classNodeLinksOfRule, conditionSet, nodeSet, conditionSetNoIdr)
		}
	}
}

func (nw *reteNetworkImpl) createFilterNode(rule model.Rule, nodesOfRule *list.List, conditionSet *list.List, nodeSet *list.List) bool {
	for e := conditionSet.Front(); e != nil; e = e.Next() {
		conditionVar := e.Value.(model.Condition)
		for f := nodeSet.Front(); f != nil; f = f.Next() {
			node := f.Value.(node)
			if ContainedByFirst(node.getIdentifiers(), conditionVar.GetIdentifiers()) {
				//TODO
				filterNode := newFilterNode(nw, nil, conditionVar)
				newNodeLink(nw, node, filterNode, false)
				removeFromList(nodeSet, node)
				nodeSet.PushBack(filterNode)
				nodesOfRule.PushBack(filterNode)
				return true
			}
		}
	}

	return false
}

func (nw *reteNetworkImpl) createJoinNodeFromExisting(rule model.Rule, nodesOfRule *list.List, conditionSet *list.List, nodeSet *list.List) bool {
	maxCommonIdr := -1
	numOfIdentifiers := 0
	joinThese := make([]node, 2)
	var targetCondition model.Condition
	for e := conditionSet.Front(); e != nil; e = e.Next() {
		conditionVar := e.Value.(model.Condition)
		for j := nodeSet.Front(); j != nil; j = j.Next() {
			leftNode := j.Value.(node)
			for k := j.Next(); k != nil; k = k.Next() {
				rightNode := k.Value.(node)
				if OtherTwoAreContainedByFirst(conditionVar.GetIdentifiers(), leftNode.getIdentifiers(), rightNode.getIdentifiers()) {
					commonIdr := len(IntersectionIdentifiers(leftNode.getIdentifiers(), rightNode.getIdentifiers()))
					if maxCommonIdr < commonIdr {
						maxCommonIdr = commonIdr
						joinThese[0] = leftNode
						joinThese[1] = rightNode
						targetCondition = conditionVar
						numOfIdentifiers = len(UnionIdentifiers(leftNode.getIdentifiers(), rightNode.getIdentifiers()))
					} else if maxCommonIdr == commonIdr {
						numIdrs := len(UnionIdentifiers(leftNode.getIdentifiers(), rightNode.getIdentifiers()))
						if numIdrs < numOfIdentifiers {
							joinThese[0] = leftNode
							joinThese[1] = rightNode
							targetCondition = conditionVar
							numOfIdentifiers = numIdrs
						}
					}
				}
			}
		}
		if maxCommonIdr != -1 {
			nw.createJoinNode(rule, nodesOfRule, joinThese[0], joinThese[1], targetCondition, conditionSet, nodeSet)
			return true
		}
	}

	return false
}

func (nw *reteNetworkImpl) createJoinNodeFromSome(rule model.Rule, nodesOfRule *list.List,
	classNodeLinksOfRule *list.List, conditionSet *list.List, nodeSet *list.List) bool {
	leastNeeded := math.MaxUint32
	maxIdentifier := -1
	var targetNode node
	var targetCondition model.Condition
	for e := conditionSet.Front(); e != nil; e = e.Next() {
		conditionVar := e.Value.(model.Condition)
		for j := nodeSet.Front(); j != nil; j = j.Next() {
			nodeIdentifiers := j.Value.(node).getIdentifiers()
			need := len(SecondMinusFirst(nodeIdentifiers, conditionVar.GetIdentifiers()))
			if need < leastNeeded {
				leastNeeded = need
				maxIdentifier = len(nodeIdentifiers)
				targetNode = j.Value.(node)
				targetCondition = conditionVar
			} else if need == leastNeeded {
				if len(nodeIdentifiers) > maxIdentifier {
					maxIdentifier = len(nodeIdentifiers)
					targetNode = j.Value.(node)
					targetCondition = conditionVar
				}
			}
		}
	}
	if maxIdentifier == -1 {
		return false
	}
	nodeIdentifiers := SecondMinusFirst(targetNode.getIdentifiers(), targetCondition.GetIdentifiers())
	if leastNeeded == 1 {
		filterNode := nw.createClassFilterNode(rule, nodesOfRule, classNodeLinksOfRule, nodeIdentifiers[0], nil, nodeSet)
		nw.createJoinNode(rule, nodesOfRule, targetNode, filterNode, targetCondition, conditionSet, nodeSet)
	} else {
		useThis := findBestNode(nodeSet, nodeIdentifiers, targetNode)
		if useThis == nil {
			nw.createClassFilterNode(rule, nodesOfRule, classNodeLinksOfRule, nodeIdentifiers[0], nil, nodeSet)
		} else {
			nw.createJoinNode(rule, nodesOfRule, targetNode, useThis, nil, conditionSet, nodeSet)
		}
	}

	return true
}

func (nw *reteNetworkImpl) createClassFilterNode(rule model.Rule, nodesOfRule *list.List, classNodeLinksOfRule *list.List, identifierVar model.TupleTypeAlias, conditionVar model.Condition, nodeSet *list.List) filterNode {
	identifiers := []model.TupleTypeAlias{identifierVar}
	classNodeVar := getClassNode(nw, identifierVar)
	filterNodeVar := newFilterNode(nw, identifiers, conditionVar)
	classNodeLink := newClassNodeLink(nw, classNodeVar, filterNodeVar, rule, identifierVar)
	classNodeVar.addClassNodeLink(classNodeLink)
	nodesOfRule.PushBack(classNodeVar)
	nodesOfRule.PushBack(filterNodeVar)
	//TODO: Add to RuleLinks
	classNodeLinksOfRule.PushBack(classNodeLink)
	nodeSet.PushBack(filterNodeVar)
	return filterNodeVar
}

func (nw *reteNetworkImpl) createJoinNode(rule model.Rule, nodesOfRule *list.List, leftNode node, rightNode node, joinCondition model.Condition, conditionSet *list.List, nodeSet *list.List) {

	//TODO handle equivJoins later..

	joinNode := newJoinNode(nw, leftNode.getIdentifiers(), rightNode.getIdentifiers(), joinCondition)

	newNodeLink(nw, leftNode, joinNode, false)
	newNodeLink(nw, rightNode, joinNode, true)
	removeFromList(nodeSet, leftNode)
	removeFromList(nodeSet, rightNode)
	nodeSet.PushBack(joinNode)
	nodesOfRule.PushBack(joinNode)
	if joinCondition != nil {
		removeFromList(conditionSet, joinCondition)
	}
}

func findBestNode(nodeSet *list.List, matchIdentifiers []model.TupleTypeAlias, notThis node) node {
	var foundNode node
	foundNode = nil
	foundIdr := 0

	for e := nodeSet.Front(); e != nil; e = e.Next() {
		node := e.Value.(node)
		if node == notThis {
			continue
		}
		foundMatch := len(IntersectionIdentifiers(node.getIdentifiers(), matchIdentifiers))
		if foundMatch > foundIdr {
			foundIdr = foundMatch
			foundNode = node
		}
	}
	return foundNode
}

func (nw *reteNetworkImpl) findConditionWithLeastIdentifiers(conditionSet *list.List) model.Condition {
	least := math.MaxUint16
	var leastIdentifiers model.Condition
	for e := conditionSet.Front(); e != nil; e = e.Next() {
		c := e.Value.(model.Condition)
		lenIdr := len(c.GetIdentifiers())
		if lenIdr < least {
			leastIdentifiers = c
			least = lenIdr
		}
	}
	if least == math.MaxUint16 {
		return nil
	}
	return leastIdentifiers
}

func getClassNode(nw *reteNetworkImpl, name model.TupleTypeAlias) classNode {
	classNodeVar := nw.allClassNodes[string(name)]
	if classNodeVar == nil {
		classNodeVar = newClassNode(string(name))
		nw.allClassNodes[string(name)] = classNodeVar
	}
	return classNodeVar
}

func (nw *reteNetworkImpl) String() string {

	str := "\n>>> Class View <<<\n"

	for _, classNodeImpl := range nw.allClassNodes {
		str += classNodeImpl.String() + "\n"
	}
	str += ">>>> Rule View <<<<\n"

	for _, rule := range nw.allRules {
		str += nw.PrintRule(rule)
	}

	return str
}

func pickIdentifier(idrs []model.TupleTypeAlias) model.TupleTypeAlias {
	return idrs[0]
}

func (nw *reteNetworkImpl) PrintRule(rule model.Rule) string {
	//str := "[Rule (" + rule.GetName() + ") Id(" + strconv.Itoa(rule.GetID()) + ")]\n"
	str := "[Rule (" + rule.GetName() + ") Id()]\n"

	nodesOfRule := nw.ruleNameNodesOfRule[rule.GetName()]

	for e := nodesOfRule.Front(); e != nil; e = e.Next() {
		node := e.Value.(abstractNode)
		switch nodeImpl := node.(type) {
		case *filterNodeImpl:
			str += nodeImpl.String()
		case *joinNodeImpl:
			str += nodeImpl.String()
		case *classNodeImpl:
			str += nw.printClassNode(rule.GetName(), nodeImpl)
		case *ruleNodeImpl:
			str += nodeImpl.String()
		}
		str += "\n"
	}
	return str
}

func (nw *reteNetworkImpl) printClassNode(ruleName string, classNodeImpl *classNodeImpl) string {
	classNodesLinksOfRule := nw.ruleNameClassNodeLinksOfRule[ruleName]
	links := ""
	for e := classNodesLinksOfRule.Front(); e != nil; e = e.Next() {
		classNodeLinkOfRule := e.Value.(classNodeLink)
		if string(classNodeLinkOfRule.GetIdentifier()) == classNodeImpl.name {
			links += "\n\t\t" + classNodeLinkOfRule.String()
		}
	}
	return "\t[ClassNode Class(" + classNodeImpl.getName() + ")" + links + "]\n"
}

func (nw *reteNetworkImpl) Assert(ctx context.Context, rs model.RuleSession, tuple model.StreamTuple) {

	if ctx == nil {
		ctx = context.Background()
	}

	reteCtxVar, isRecursive, newCtx := getOrSetReteCtx(ctx, nw, rs)

	if !isRecursive {
		nw.crudLock.Lock()
		defer nw.crudLock.Unlock()
		nw.assertInternal(newCtx, tuple)
	} else {
		reteCtxVar.getOpsList().PushBack(newAssertEntry(tuple))
	}

	reteCtxVar.getConflictResolver().resolveConflict(newCtx)
}

func (nw *reteNetworkImpl) Retract(ctx context.Context, tuple model.StreamTuple) {

	if ctx == nil {
		ctx = context.Background()
	}
	_, isRecursive, _:= getOrSetReteCtx(ctx, nw, nil)

	if !isRecursive {
		nw.crudLock.Lock()
		defer nw.crudLock.Unlock()
	}
	reteHandle := nw.allHandles[tuple]
	if reteHandle != nil {
		reteHandle.removeJoinTableRowRefs()
	}

}

func (nw *reteNetworkImpl) assertInternal(ctx context.Context, tuple model.StreamTuple) {
	dataSource := tuple.GetTypeAlias()
	listItem := nw.allClassNodes[string(dataSource)]
	if listItem != nil {
		classNodeVar := listItem.(classNode)
		classNodeVar.assert(ctx, tuple)
	} else {
		fmt.Println("No rule exists for data stream: " + dataSource)
	}
}

func (nw *reteNetworkImpl) getOrCreateHandle(tuple model.StreamTuple) reteHandle {
	h := nw.allHandles[tuple]
	if h == nil {
		h1 := handleImpl{}
		h1.initHandleImpl()
		h1.setTuple(tuple)
		h = &h1
		nw.allHandles[tuple] = h
	}
	return h
}

func (nw *reteNetworkImpl) incrementAndGetId() int {
	nw.currentId++
	return nw.currentId
}