package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"github.com/emicklei/dot"
)

// dump exports the entire access graph.
func dump(ag *AccessGraph) error {
	b, err := json.Marshal(ag)
	if err != nil {
		return err
	}
	filename := fmt.Sprintf("rbiam-dump-%v.json", time.Now().Unix())
	err = ioutil.WriteFile(filename, b, 0644)
	return err
}

// load imports access graph from filename
func load(filename string) (*AccessGraph, error) {
	ag := &AccessGraph{}
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		return ag, err
	}
	err = json.Unmarshal(b, ag)
	return ag, err
}

// exportRaw exports the trace as a raw dump in JSON format into a file
// in the current working directory with a name of 'rbiam-trace-NNNNNNNNNN' with
// the NNNNNNNNNN being the Unix timestamp of the creation time, for example:
// rbiam-trace-1564315687.json
func exportRaw(trace []string, ag *AccessGraph) (string, error) {
	dump := ""
	for _, item := range trace {
		itype, ikey := extractTK(item)
		switch itype {
		case "IAM role":
			b, err := json.Marshal(ag.Roles[ikey])
			if err != nil {
				return "", err
			}
			dump = fmt.Sprintf("%v\n%v", dump, string(b))
		case "IAM policy":
			b, err := json.Marshal(ag.Policies[ikey])
			if err != nil {
				return "", err
			}
			dump = fmt.Sprintf("%v\n%v", dump, string(b))
		case "Kubernetes service account":
			b, err := json.Marshal(ag.ServiceAccounts[ikey])
			if err != nil {
				return "", err
			}
			dump = fmt.Sprintf("%v\n%v", dump, string(b))
		case "Kubernetes secret":
			b, err := json.Marshal(ag.Secrets[ikey])
			if err != nil {
				return "", err
			}
			dump = fmt.Sprintf("%v\n%v", dump, string(b))
		case "Kubernetes pod":
			b, err := json.Marshal(ag.Pods[ikey])
			if err != nil {
				return "", err
			}
			dump = fmt.Sprintf("%v\n%v", dump, string(b))
		}
	}

	filename := fmt.Sprintf("rbiam-trace-%v.json", time.Now().Unix())
	err := ioutil.WriteFile(filename, []byte(dump), 0644)
	if err != nil {
		return "", err
	}
	return filename, nil
}

// exportGraph exports the trace as a graph in DOT format into a file
// in the current working directory with a name of 'rbiam-trace-NNNNNNNNNN' with
// the NNNNNNNNNN being the Unix timestamp of the creation time, for example:
// rbiam-trace-1564315687.dot
func exportGraph(trace []string, ag *AccessGraph) (string, error) {
	g := dot.NewGraph(dot.Directed)
	// make sure the legend is at the bottom:
	g.Attr("newrank", "true")
	// legend:
	legend := g.Subgraph("LEGEND", dot.ClusterOption{})
	lsa := formatAsServiceAccount(legend.Node("Kubernetes service account"))
	lsecret := formatAsSecret(legend.Node("Kubernetes secret"))
	lpod := formatAsPod(legend.Node("Kubernetes pod"))
	lrole := formatAsRole(legend.Node("IAM role"))
	lpolicy := formatAsPolicy(legend.Node("IAM policy"))
	legend.Edge(lpod, lsa, "uses").Attr("fontname", "Helvetica")
	legend.Edge(lsa, lsecret, "has").Attr("fontname", "Helvetica")
	legend.Edge(lrole, lpolicy, "has").Attr("fontname", "Helvetica")
	legend.Edge(lpod, lrole, "assumes").Attr("fontname", "Helvetica")

	// first let's draw the nodes and remember the
	// graph entry points for traversals to later draw
	// the edges starting with Kubernetes pods and IAM roles:
	pods := make(map[string]dot.Node)
	sas := make(map[string]dot.Node)
	secrets := make(map[string]dot.Node)
	roles := make(map[string]dot.Node)
	policies := make(map[string]dot.Node)
	for _, item := range trace {
		itype, ikey := extractTK(item)
		switch itype {
		case "IAM role":
			roles[ikey] = formatAsRole(g.Node(ikey))
		case "IAM policy":
			policies[ikey] = formatAsPolicy(g.Node(ikey))
		case "Kubernetes service account":
			sas[ikey] = formatAsServiceAccount(g.Node(ikey))
		case "Kubernetes secret":
			secrets[ikey] = formatAsSecret(g.Node(ikey))
		case "Kubernetes pod":
			pods[ikey] = formatAsPod(g.Node(ikey))
		}
	}

	// next, we draw the edges:
	// pods -> service accounts
	for podname, node := range pods {
		for _, item := range trace {
			itype, ikey := extractTK(item)
			if itype == "Kubernetes service account" {
				podsa := namespaceit(ag.Pods[podname].Namespace, ag.Pods[podname].Spec.ServiceAccountName)
				if podsa == ikey {
					g.Edge(node, sas[ikey])
				}
			}
		}
	}
	// service accounts -> secrets
	for saname, node := range sas {
		for _, item := range trace {
			itype, ikey := extractTK(item)
			if itype == "Kubernetes secret" {
				// for now we simply take the first secret of the service account, should really iterate over all and check each:
				sasecrect := namespaceit(ag.ServiceAccounts[saname].Namespace, ag.ServiceAccounts[saname].Secrets[0].Name)
				if sasecrect == ikey {
					g.Edge(node, secrets[ikey])
				}
			}
		}
	}
	// pods -> IAM roles
	for podname, node := range pods {
		for _, item := range trace {
			itype, ikey := extractTK(item)
			if itype == "IAM role" {
				// for IRP-enabled pods:
				for _, container := range ag.Pods[podname].Spec.Containers {
					for _, envar := range container.Env {
						if envar.Name == "AWS_ROLE_ARN" && envar.Value == ikey {
							g.Edge(node, roles[ikey])
						}
					}
				}
				// for traditional, node-level IAM role assignment:
				// iterate over EC2 instances and select the ones where the
				// pods' hostIP matches, then take the EC2 NodeInstanceRole
			}
		}
	}

	// IAM roles -> IAM policies
	// https://godoc.org/github.com/aws/aws-sdk-go-v2/service/iam#Client.ListAttachedRolePoliciesRequest

	// now we can write out the graph into a file in DOT format:
	filename := fmt.Sprintf("rbiam-trace-%v.dot", time.Now().Unix())
	err := ioutil.WriteFile(filename, []byte(g.String()), 0644)
	if err != nil {
		return "", err
	}
	return filename, nil
}

// extractTK takes a history item in the form [TYPE] KEY
// and return t as the TYPE and k as the KEY, for example:
// [Kubernetes service account] default:s3-echoer ->
// t == Kubernetes service account
// k == default:s3-echoer
func extractTK(item string) (t, k string) {
	t = strings.TrimPrefix(strings.Split(item, "]")[0], "[")
	k = strings.TrimSpace(strings.Split(item, "]")[1])
	return
}

func formatAsRole(n dot.Node) dot.Node {
	return n.Attr("style", "filled").Attr("fillcolor", "#FD8564").Attr("fontcolor", "#000000").Attr("fontname", "Helvetica")
}

func formatAsPolicy(n dot.Node) dot.Node {
	return n.Attr("style", "filled").Attr("fillcolor", "#D9A7F1").Attr("fontcolor", "#000000").Attr("fontname", "Helvetica")
}

func formatAsServiceAccount(n dot.Node) dot.Node {
	return n.Attr("style", "filled").Attr("fillcolor", "#1BFF9F").Attr("fontcolor", "#000000").Attr("fontname", "Helvetica")
}

func formatAsSecret(n dot.Node) dot.Node {
	return n.Attr("style", "filled").Attr("fillcolor", "#F9ED49").Attr("fontcolor", "#000000").Attr("fontname", "Helvetica")
}

func formatAsPod(n dot.Node) dot.Node {
	return n.Attr("style", "filled").Attr("fillcolor", "#4260FA").Attr("fontcolor", "#f0f0f0").Attr("fontname", "Helvetica")
}
