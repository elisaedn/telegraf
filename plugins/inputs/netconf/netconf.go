package netconf

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/logger"
	"github.com/influxdata/telegraf/plugins/inputs"
)

//go:embed sample.conf
var sampleConfig string

const retryWait = 5 * time.Second

// TelegrafRecord contains parsed data from input plugin to be added to accumulator
type TelegrafRecord struct {
	Name      string
	Tags      map[string]string
	Fields    map[string]any
	Timestamp time.Time
}

// Subtree defines an RPC get command with a subtree filter and a path to a reference element within the reply.
type Subtree struct {
	Name           string `toml:"name"`
	Subtree        string `toml:"subtree"`
	WithDefaults   string `toml:"with_defaults"`
	Path           string `toml:"path"`
	Datastore      string `toml:"datastore"`
	Method         string `toml:"rpc_method"`
	RemoveNewlines bool   `toml:"remove_newlines"`
	Skip           bool   `toml:"skip"`
}

// Target defines how the named (by `name`) metric is extracted.
type Target struct {
	Name    string           `toml:"name"`
	Subtree string           `toml:"subtree"`
	Path    string           `toml:"path"`
	Tags    []map[string]any `toml:"tags"`
	Fields  []map[string]any `toml:"fields"`
}

// Netconf contains the list of endpoint and subtree configurations for collecting
// configuration information over netconf.
type Netconf struct {
	Period    string            `toml:"period"`
	Username  string            `toml:"username"`
	Password  config.Secret     `toml:"password"`
	Addresses []string          `toml:"addresses"`
	Parallel  int               `toml:"endpoints_in_parallel"`
	Retries   int               `toml:"retries"`
	Subtrees  []Subtree         `toml:"subtree"`
	Targets   []Target          `toml:"target"`
	GetClient func() Connection `toml:"-"`
	Log       telegraf.Logger   `toml:"-"`

	cancel context.CancelFunc
}

type sessionContext struct {
	endpoint endpoint
	subtrees map[string]*xmlquery.Node
	acc      telegraf.Accumulator
	records  atomic.Uint32
}

type endpoint struct {
	host     string
	port     string
	username string
	password config.Secret

	log telegraf.Logger
}

func init() {
	xmlquery.DisableSelectorCache = false
	inputs.Add("netconf", func() telegraf.Input {
		return &Netconf{
			Retries: 3,
			GetClient: func() Connection {
				return new(Client)
			},
		}
	})
}

// SampleConfig returns the sample configuration of the plugin
func (*Netconf) SampleConfig() string {
	return sampleConfig
}

func (p *Netconf) Init() error {
	if len(p.Targets) == 0 {
		return &internal.FatalError{Err: errors.New("no targets defined")}
	}

	if len(p.Subtrees) == 0 {
		return &internal.FatalError{Err: errors.New("no subtrees defined")}
	}

	for _, subtree := range p.Subtrees {
		if !slices.Contains([]string{"", "get", "get-config", "rpc"}, subtree.Method) {
			return &internal.FatalError{Err: fmt.Errorf("invalid method %s for subtree %s", subtree.Method, subtree.Name)}
		}

		if !slices.Contains([]string{"", "report-all", "report-all-tagged", "trim", "explicit"}, subtree.WithDefaults) {
			return &internal.FatalError{Err: fmt.Errorf("invalid with_defaults %s for subtree %s", subtree.WithDefaults, subtree.Name)}
		}
	}
	return nil
}

// Gather collects configuration data of listed devices
func (p *Netconf) Gather(acc telegraf.Accumulator) error {
	var ctx context.Context
	ctx, p.cancel = context.WithCancel(context.Background())
	p.Log.Infof("Starting gather-cycle with %d endpoints: %v", len(p.Addresses), p.Addresses)

	sem := make(chan struct{}, p.Parallel)
	defer close(sem)

	var wg sync.WaitGroup
	wg.Add(len(p.Addresses))
	for _, address := range p.Addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}

		log := logger.New("inputs", "netconf", "")
		log.AddAttribute("routerIp", host)
		endpointToCollect := endpoint{
			host:     host,
			port:     port,
			username: p.Username,
			password: p.Password,
			log:      log,
		}
		sem <- struct{}{}

		go func(endpoint endpoint) {
			defer wg.Done()
			p.collectEndpoint(ctx, endpoint, acc)
			<-sem
		}(endpointToCollect)
	}
	wg.Wait()

	p.Log.Infof("Gather-cycle completed.")
	return nil
}

func (p *Netconf) collectEndpoint(ctx context.Context, endpoint endpoint, acc telegraf.Accumulator) {
	cl := &sessionContext{
		endpoint: endpoint,
		acc:      acc,
		subtrees: make(map[string]*xmlquery.Node, len(p.Subtrees)),
	}

	nc := p.GetClient()
	nc.SetLogger(endpoint.log)

	err := nc.Connect(ctx, fmt.Sprintf("%s:%s", endpoint.host, endpoint.port), endpoint.username, endpoint.password)
	if err != nil {
		endpoint.log.Errorf("Failed to connect: %v", err)
		acc.AddError(err)
		return
	}
	endpoint.log.Infof("Connected with sessionId %d", nc.SessionID())
	closeSession := func() {
		if err := nc.Close(); err != nil {
			endpoint.log.Warnf("Failed to close session: %v", err)
		}
	}
	defer closeSession()

	endpoint.log.Infof("Collecting endpoint %s", endpoint.host)
	cl.subtrees, err = nc.GetSubtrees(ctx, p.Retries, p.Subtrees)
	switch {
	case err != nil:
		endpoint.log.Errorf("Subtree collecting failed: %v", err)
		acc.AddError(fmt.Errorf("failed to collect subtrees for endpoint %s: %v", endpoint.host, err))
	case len(cl.subtrees) == 0:
		endpoint.log.Errorf("Expected to collect %d subtrees, collected 0", len(p.Subtrees))
		acc.AddError(fmt.Errorf("zero subtrees collected from endpoint %s: %v", endpoint.host, err))
	default:
		cl.collectTargets(p.Targets)

	}
}

func (c *sessionContext) collectTargets(targets []Target) {
	start := time.Now()
	for _, target := range targets {
		root, found := c.subtrees[target.Subtree]
		if !found {
			c.endpoint.log.Debugf("Skipping target %s, subtree %s not found", target.Name, target.Subtree)
			continue
		}

		c.collectTarget(target, root)
	}
	c.endpoint.log.Infof("Collected %d records in %s.", c.records.Load(), time.Now().Sub(start))
}

func (c *sessionContext) collectTarget(target Target, root *xmlquery.Node) {
	var (
		wg          sync.WaitGroup
		targetNodes = xmlquery.Find(root, target.Path)
	)

	wg.Add(len(targetNodes))
	for _, targetNode := range targetNodes {
		go func() {
			defer wg.Done()
			c.collectTargetAtElement(target, targetNode)
		}()
	}
	wg.Wait()
	c.endpoint.log.Debugf("Collected %d %s records", len(targetNodes), target.Name)

}

func (c *sessionContext) collectTargetAtElement(target Target, targetNode *xmlquery.Node) {
	record := TelegrafRecord{
		Name:      target.Name,
		Tags:      map[string]string{"source": c.endpoint.host},
		Fields:    make(map[string]any),
		Timestamp: time.Now().UTC(),
	}
	for table := range slices.Values(target.Tags) {
		for tagName, value := range collectTable(c, targetNode, table, record.Tags) {
			record.Tags[tagName] = value.(string)
		}
	}
	for table := range slices.Values(target.Fields) {
		for fieldName, value := range collectTable(c, targetNode, table, record.Tags) {
			record.Fields[fieldName] = value
		}
	}

	c.acc.AddFields(record.Name, record.Fields, record.Tags, record.Timestamp)
	c.records.Add(1)
}

func collectTable(context *sessionContext, targetNode *xmlquery.Node, table map[string]any, tags map[string]string) map[string]any {
	var (
		matchingNodes = getElements(context, targetNode, table, tags)
		result        = make(map[string]any)
	)
	if collectAll, ok := table["_all"].(bool); ok && collectAll {
		for node := range slices.Values(matchingNodes) {
			getAllValues(node, "", result)
		}
	}

	separator := table["_append"]
	for name, rowSpec := range table {
		if !strings.HasPrefix(name, "_") {
			rowValue := getRowValue(rowSpec, result[name], matchingNodes, separator, tags)
			if rowValue != nil {
				result[name] = rowValue
			}
		}
	}
	return result
}

func getAllValues(node *xmlquery.Node, prefix string, fields map[string]any) {
	if len(prefix) == 0 && node.Type != xmlquery.ElementNode {
		return
	}
	if isTextElement(node) {
		fields[prefix] = strings.TrimSpace(node.InnerText())
		return
	}
	for elem := node.FirstChild; elem != nil; elem = elem.NextSibling {
		newPrefix := elem.Data
		if len(prefix) > 0 {
			newPrefix = strings.Join([]string{prefix, elem.Data}, "/")
		}
		getAllValues(elem, newPrefix, fields)
	}
}

func getElements(context *sessionContext, elem *xmlquery.Node, table map[string]any, tags map[string]string) []*xmlquery.Node {
	elems := []*xmlquery.Node{elem}
	if subtreeName, found := table["_subtree"].(string); found {
		if subtreeNode, found := context.subtrees[subtreeName]; found {
			elems = []*xmlquery.Node{subtreeNode}
		} else {
			context.endpoint.log.Debugf("Subtree %s not found, skipping table", subtreeName)
			return []*xmlquery.Node{}
		}
	}
	if path, found := table["_path"].(string); found {
		for name, value := range tags {
			path = strings.ReplaceAll(path, fmt.Sprintf("{%s}", name), value)
		}
		elems = xmlquery.Find(elems[0], path)
	}
	return elems
}

func getRowValue(rowSpec, rowValue any, nodes []*xmlquery.Node, separator any, tags map[string]string) any {

	for _, node := range nodes {
		if value := getValue(node, rowSpec, tags); value != nil {
			if separator != nil && rowValue != nil {
				rowValue = fmt.Sprintf("%v%v%v", rowValue, separator, value)
			} else {
				rowValue = value
			}
		}
	}
	return rowValue
}

func getValue(node *xmlquery.Node, value any, tags map[string]string) any {
	v, ok := value.(string)
	if !ok {
		return value
	}
	for tagName, tagValue := range tags {
		v = strings.ReplaceAll(v, fmt.Sprintf("{%s}", tagName), tagValue)
	}
	switch {
	case strings.HasPrefix(v, "./"):
		textNode := xmlquery.FindOne(node, v)
		if textNode == nil {
			return nil // value was not found
		}
		if isTextElement(textNode) {
			return textNode.InnerText()
		}
		return textNode.OutputXML(true)
	case isXpathFunction(v):
		evaluated := xpath.MustCompile(v).Evaluate(xmlquery.CreateXPathNavigator(node))
		switch ev := evaluated.(type) {
		case xpath.NodeIterator:
			return nil
		case float64:
			if math.IsNaN(ev) {
				return nil
			}
			return ev
		default:
			return ev
		}
	default:
		return v
	}
}

func isTextElement(node *xmlquery.Node) bool {
	if node == nil || node.Type != xmlquery.ElementNode {
		return false
	}
	if node.FirstChild == nil {
		return true // empty element is considered to contain empty text
	}
	if node.LastChild != node.FirstChild {
		return false // multiple child elements
	}
	if node.FirstChild.Type != xmlquery.TextNode {
		return false // only child element is not a text element
	}
	return true
}

func isXpathFunction(value string) bool {
	return strings.HasPrefix(value, "concat") ||
		strings.HasPrefix(value, "substring") ||
		strings.HasPrefix(value, "boolean") ||
		strings.HasPrefix(value, "number") ||
		strings.HasPrefix(value, "string") ||
		strings.HasPrefix(value, "replace") ||
		strings.HasPrefix(value, "reverse") ||
		strings.HasPrefix(value, "sum") ||
		strings.HasPrefix(value, "count") ||
		strings.HasPrefix(value, "name(") ||
		strings.HasPrefix(value, "local-name")
}
