package model

// RuleGroupIDs includes every hop and recursively referenced failover group.
func RuleGroupIDs(rule *ForwardRule, groups map[uint64]*DeviceGroup) map[uint64]bool {
	ids := map[uint64]bool{}
	var visit func(uint64)
	visit = func(id uint64) {
		if id == 0 || ids[id] {
			return
		}
		ids[id] = true
		if g := groups[id]; g != nil {
			visit(g.FailoverGroupID)
		}
	}
	visit(rule.InboundGroupID)
	if chain := rule.ChainGroupList(); len(chain) != 0 {
		for _, id := range chain {
			visit(id)
		}
	} else if rule.ReverseEnable && rule.ReverseGroupID != 0 {
		visit(rule.ReverseGroupID)
	} else {
		visit(rule.OutboundGroupID)
	}
	return ids
}

func RuleNodeRoles(rule *ForwardRule, nodeID uint64, groups map[uint64]*DeviceGroup) (inbound, outbound bool) {
	for id := range RuleGroupIDs(rule, groups) {
		g := groups[id]
		if g == nil {
			continue
		}
		for _, member := range g.NodeIDs.AsUint64Slice() {
			if member != nodeID {
				continue
			}
			if id == rule.InboundGroupID {
				inbound = true
			} else {
				outbound = true
			}
		}
	}
	return
}
