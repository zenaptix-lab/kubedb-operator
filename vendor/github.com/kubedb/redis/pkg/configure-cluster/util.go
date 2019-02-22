package configure_cluster

import (
	"fmt"
	"strconv"
	"strings"

	api "github.com/kubedb/apimachinery/apis/kubedb/v1alpha1"
)

func getMyConf(nodesConf string) (myConf string) {
	myConf = ""
	nodes := strings.Split(nodesConf, "\n")
	for _, node := range nodes {
		if strings.Contains(node, "myself") {
			myConf = strings.TrimSpace(node)
			break
		}
	}

	return myConf
}

func getNodeConfByIP(nodesConf, ip string) (myConf string) {
	myConf = ""
	nodes := strings.Split(nodesConf, "\n")
	for _, node := range nodes {
		if strings.Contains(node, ip) {
			myConf = strings.TrimSpace(node)
			break
		}
	}

	return myConf
}

func getNodeId(nodeConf string) string {
	return strings.Split(nodeConf, " ")[0]
}

func getNodeRole(nodeConf string) (nodeRole string) {
	nodeRole = ""
	if strings.Contains(nodeConf, "master") {
		nodeRole = "master"
	} else if strings.Contains(nodeConf, "slave") {
		nodeRole = "slave"
	}

	return nodeRole
}

func getMasterID(nodeConf string) (masterID string) {
	masterID = ""
	if getNodeRole(nodeConf) == "slave" {
		masterID = strings.Split(nodeConf, " ")[3]
	}

	return masterID
}

// processNodesConf stores nodes info into a map from nodesConf in the order they are in nodes.conf file
func processNodesConf(nodesConf string) map[string]*RedisNode {
	var (
		slotRange  []string
		start, end int
		nds        map[string]*RedisNode
	)

	nds = make(map[string]*RedisNode)
	nodes := strings.Split(nodesConf, "\n")

	for _, node := range nodes {
		node = strings.TrimSpace(node)
		parts := strings.Split(strings.TrimSpace(node), " ")

		if strings.Contains(parts[2], "noaddr") {
			continue
		}

		if strings.Contains(parts[2], "master") {
			nd := RedisNode{
				ID:   parts[0],
				IP:   strings.Split(parts[1], ":")[0],
				Port: api.RedisNodePort,
				Role: "master",
				Down: false,
			}
			if strings.Contains(parts[2], "fail") {
				nd.Down = true
			}
			nd.SlotsCnt = 0
			for j := 8; j < len(parts); j++ {
				if parts[j][0] == '[' && parts[j][len(parts[j])-1] == ']' {
					continue
				}

				slotRange = strings.Split(parts[j], "-")
				start, _ = strconv.Atoi(slotRange[0])
				if len(slotRange) == 1 {
					end = start
				} else {
					end, _ = strconv.Atoi(slotRange[1])
				}

				nd.SlotStart = append(nd.SlotStart, start)
				nd.SlotEnd = append(nd.SlotEnd, end)
				nd.SlotsCnt += (end - start) + 1
			}
			nd.Slaves = []*RedisNode{}

			nds[nd.ID] = &nd
		}
	}

	for _, node := range nodes {
		node = strings.TrimSpace(node)
		parts := strings.Split(strings.TrimSpace(node), " ")

		if strings.Contains(parts[2], "noaddr") {
			continue
		}

		if strings.Contains(parts[2], "slave") {
			nd := RedisNode{
				ID:   parts[0],
				IP:   strings.Split(parts[1], ":")[0],
				Port: api.RedisNodePort,
				Role: "slave",
				Down: false,
			}
			if strings.Contains(parts[2], "fail") {
				nd.Down = true
			}
			nd.Master = nds[parts[3]]
			nds[parts[3]].Slaves = append(nds[parts[3]].Slaves, &nd)
		}
	}

	return nds
}

func nodeAddress(ip string) string {
	return fmt.Sprintf("%s:%d", ip, api.RedisNodePort)
}
