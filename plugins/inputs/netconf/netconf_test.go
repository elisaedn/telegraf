package netconf

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/antchfx/xmlquery"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/testutil"
	"github.com/networkguild/netconf"
	"github.com/stretchr/testify/require"
)

func init() {
	xmlquery.DisableSelectorCache = true
}

// MockNetconfServer implements Connection and can be used in testing netconf -plugin
type MockNetconfServer struct {
	Connected    bool
	Reply        map[string]string
	FailCount    int
	ConnectError error
	ExecError    error
}

func (client *MockNetconfServer) SetLogger(_ telegraf.Logger) {}

// Connect initializes fake netconf connection
func (client *MockNetconfServer) GetSubtrees(ctx context.Context, retries int, subtrees []Subtree) (map[string]*xmlquery.Node, error) {
	collected := make(map[string]*xmlquery.Node, len(subtrees))
	for _, subtree := range subtrees {
		node, skipped, err := getWithRetry(ctx, subtree, retries, client.GetSubtree)
		switch {
		case skipped:
		case err != nil:
			return collected, fmt.Errorf("failed to collect subtree %s, error: %v", subtree.Name, err)
		default:
			collected[subtree.Name] = node
		}
	}
	return collected, nil
}

// Connect initializes fake netconf connection
func (client *MockNetconfServer) Connect(context.Context, string, string, config.Secret) error {
	client.Connected = true
	return client.ConnectError
}

// Close ends fake netconf connection
func (client *MockNetconfServer) Close() error {
	client.Connected = false
	return nil
}

func (client *MockNetconfServer) SessionID() uint64 {
	return 0
}

func (client *MockNetconfServer) GetSubtree(_ context.Context, subtree Subtree) (*xmlquery.Node, error) {
	if !client.Connected {
		err := fmt.Errorf("no connection")
		return nil, err
	} else if client.FailCount > 0 {
		err := client.ExecError
		client.FailCount--
		return nil, err
	}

	reply, found := client.Reply[subtree.Subtree]
	if !found {
		err := fmt.Errorf("undefined filter: %s", subtree.Subtree)
		return nil, err
	}

	return parseNodeFromData(subtree.Path, []byte(reply), false)
}

const (
	devmSubtree = "<devm xmlns=\"urn:huawei:yang:huawei-devm\"><physical-entitys/></devm>"
	ifmSubtree  = "<ifm xmlns=\"urn:huawei:yang:huawei-ifm\"><interfaces/></ifm>"

	devmReply = `<rpc-reply>
  <data>
    <devm xmlns="urn:huawei:yang:huawei-devm">
      <physical-entitys>
        <physical-entity>
          <class>port</class>
          <name>name1</name>
          <prop1>value11</prop1>
          <prop2>value12</prop2>
          <int>112</int>
          <bool>true</bool>
        </physical-entity>
        <physical-entity>
          <class>port</class>
          <name>name2</name>
          <prop1>value21</prop1>
          <prop2>value22</prop2>
          <bool>false</bool>
        </physical-entity>
        <physical-entity>
          <class>port</class>
          <name>test-all</name>
        </physical-entity>
      </physical-entitys>
    </devm>
  </data>
</rpc-reply>`

	ifmReply = `<rpc-reply>
  <data>
    <ifm xmlns="urn:huawei:yang:huawei-ifm">
      <interfaces>
        <interface>
          <class>main-interface</class>
          <name>name1</name>
          <prop3>value13</prop3>
          <prop4>value14</prop4>
        </interface>
        <interface>
          <class>main-interface</class>
          <name>name2</name>
          <prop2>value22_new</prop2>
          <prop5>value25</prop5>
        </interface>
        <interface>
          <class>main-interface</class>
          <name>test-all</name>
          <all1>value_all1</all1>
          <all2>value_all2</all2>
        </interface>
      </interfaces>
    </ifm>
  </data>
</rpc-reply>`
)

var (
	mockReplyMap  = map[string]string{devmSubtree: devmReply, ifmSubtree: ifmReply}
	mockServer    = MockNetconfServer{Reply: mockReplyMap}
	newMockServer = func() Connection { return &mockServer }
	Logger        = testutil.Logger{Name: "inputs.netconf"}
)

func toTelegrafRecord(m *testutil.Metric) TelegrafRecord {
	return TelegrafRecord{
		Name:      m.Measurement,
		Tags:      m.Tags,
		Fields:    m.Fields,
		Timestamp: m.Time,
	}
}

func assertRecords(t *testing.T, expected, actual map[string]TelegrafRecord) {
	require.Equal(t, len(expected), len(actual), "number of records")
	for name, expectedRecord := range expected {
		actualRecord, found := actual[name]
		require.True(t, found, fmt.Sprintf("actual record '%s' exists", name))
		assertRecord(t, expectedRecord, actualRecord, fmt.Sprintf("Record %s: ", name))
	}
}

func assertRecord(t *testing.T, expected, actual TelegrafRecord, label string) {
	require.Equal(t, expected.Name, actual.Name, fmt.Sprintf("%smeasurement name", label))
	require.Equal(t, len(expected.Tags), len(actual.Tags), fmt.Sprintf("%snumber of tags", label))
	require.Equal(t, len(expected.Fields), len(actual.Fields), fmt.Sprintf("%snumber of fields", label))
	for tagName, expectedValue := range expected.Tags {
		actualValue, found := actual.Tags[tagName]
		require.True(t, found, fmt.Sprintf("%sactual tag '%s' exists", label, tagName))
		require.Equal(t, expectedValue, actualValue, fmt.Sprintf("%svalue of tag %s", label, tagName))
	}
	for fieldName, expectedValue := range expected.Fields {
		actualValue, found := actual.Fields[fieldName]
		require.True(t, found, fmt.Sprintf("%sactual field '%s' exists", label, fieldName))
		require.EqualValues(t, expectedValue, actualValue, fmt.Sprintf("%svalue of field %s", label, fieldName))
	}
}

func TestCollectSubtrees(t *testing.T) {
	ctx := &sessionContext{
		endpoint: endpoint{host: "10.0.0.1", port: "830", log: Logger},
		subtrees: map[string]*xmlquery.Node{},
	}
	subtrees := []Subtree{
		{
			Name:    "devm",
			Subtree: devmSubtree,
			Path:    "/rpc-reply/data/devm/physical-entitys",
		},
		{
			Name:    "ifm",
			Subtree: ifmSubtree,
			Path:    "/data/ifm/interfaces",
		},
	}

	var (
		err            error
		mockConnection = newMockServer()
	)
	require.NoError(t, mockConnection.Connect(t.Context(), "", "", *new(config.Secret)))
	ctx.subtrees, err = mockConnection.GetSubtrees(t.Context(), 0, subtrees)
	require.NoError(t, err)

	node, found := ctx.subtrees["devm"]
	require.True(t, found)
	require.NotNil(t, node)
	require.Equal(t, "physical-entitys", node.Data)
	for elem := node.FirstChild; elem != nil; elem = elem.NextSibling {
		if elem.Type == xmlquery.ElementNode {
			require.Equal(t, "physical-entity", elem.Data)
		}
	}

	node, found = ctx.subtrees["ifm"]
	require.True(t, found)
	require.Equal(t, "interfaces", node.Data)
	for elem := node.FirstChild; elem != nil; elem = elem.NextSibling {
		if elem.Type == xmlquery.ElementNode {
			require.Equal(t, "interface", elem.Data)
		}
	}
}

func TestCollectTargets(t *testing.T) {
	acc := new(testutil.Accumulator)
	ctx := &sessionContext{
		endpoint: endpoint{host: "10.0.0.1", port: "830", log: Logger},
		subtrees: map[string]*xmlquery.Node{},
		acc:      acc,
	}
	subtrees := []Subtree{
		{Name: "devm", Subtree: devmSubtree, Path: "/data/devm/physical-entitys"},
		{Name: "ifm", Subtree: ifmSubtree, Path: "/data/ifm/interfaces"},
	}

	var (
		err            error
		mockConnection = newMockServer()
	)
	require.NoError(t, mockConnection.Connect(t.Context(), "", "", *new(config.Secret)))
	ctx.subtrees, err = mockConnection.GetSubtrees(t.Context(), 1, subtrees)
	require.NoError(t, err)

	targets := []Target{
		{
			Name:    "testPort",
			Subtree: "devm",
			Path:    "physical-entity[class='port']",
			Tags: []map[string]any{
				{"name": "./name"},
			},
			Fields: []map[string]any{
				{
					"prop0": 123,
					"_all":  true,
				},
				{
					"_subtree": "ifm",
					"_path":    "./interface[class='main-interface' and name='{name}']",
					"prop2":    "concat(/prop2, '+', /int)",
					"prop3":    "./prop3",
					"prop4":    "./prop4",
					"prop5":    "./prop5",
				},
				{
					"_subtree": "ifm",
					"_path":    "./interface[class='main-interface' and name='{name}']",
					"_all":     true,
				},
			},
		},
	}

	ctx.collectTargets(targets)

	actual := make(map[string]TelegrafRecord, len(acc.Metrics))
	for _, m := range acc.Metrics {
		actual[fmt.Sprintf("%s:%s", m.Measurement, m.Tags["name"])] = toTelegrafRecord(m)
	}
	expected := map[string]TelegrafRecord{
		"testPort:name1": {
			Name: "testPort",
			Tags: map[string]string{
				"source": ctx.endpoint.host,
				"name":   "name1",
			},
			Fields: map[string]any{
				"class": "main-interface",
				"name":  "name1",
				"prop0": 123,
				"prop1": "value11",
				"prop2": "+",
				"prop3": "value13",
				"prop4": "value14",
				"bool":  "true",
				"int":   "112",
			},
		},
		"testPort:name2": {
			Name: "testPort",
			Tags: map[string]string{
				"source": ctx.endpoint.host,
				"name":   "name2",
			},
			Fields: map[string]any{
				"class": "main-interface",
				"name":  "name2",
				"prop0": 123,
				"prop1": "value21",
				"prop2": "value22_new",
				"prop5": "value25",
				"bool":  "false",
			},
		},
		"testPort:test-all": {
			Name: "testPort",
			Tags: map[string]string{
				"source": ctx.endpoint.host,
				"name":   "test-all",
			},
			Fields: map[string]any{
				"class": "main-interface",
				"name":  "test-all",
				"prop0": 123,
				"prop2": "+",
				"all1":  "value_all1",
				"all2":  "value_all2",
			},
		},
	}

	assertRecords(t, expected, actual)
}

func TestAppending(t *testing.T) {
	acc := new(testutil.Accumulator)
	ctx := &sessionContext{
		endpoint: endpoint{host: "10.0.0.1", port: "830", log: Logger},
		subtrees: make(map[string]*xmlquery.Node),
		acc:      acc,
	}
	subtrees := []Subtree{
		{Name: "devm", Subtree: devmSubtree, Path: "/data/devm/physical-entitys"},
	}

	var (
		err            error
		mockConnection = newMockServer()
	)
	require.NoError(t, mockConnection.Connect(t.Context(), "", "", *new(config.Secret)))
	ctx.subtrees, err = mockConnection.GetSubtrees(t.Context(), 1, subtrees)
	require.NoError(t, err)

	targets := []Target{
		{
			Name:    "testPort",
			Subtree: "devm",
			Path:    ".",
			Tags: []map[string]any{
				{
					"_path":   "physical-entity[class='port']",
					"_append": "+",
					"name":    "./name",
				},
			},
			Fields: []map[string]any{
				{
					"_path":   "physical-entity[class='port']",
					"_append": ",",
					"prop1":   "./prop1",
				},
				{
					"_path":   "physical-entity[class='port']/prop2",
					"_append": ";",
					"prop2":   "./.",
				},
			},
		},
	}

	ctx.collectTargets(targets)

	actual := make(map[string]TelegrafRecord)
	for _, m := range acc.Metrics {
		actual[fmt.Sprintf("%s:%s", m.Measurement, m.Tags["name"])] = toTelegrafRecord(m)
	}
	expected := map[string]TelegrafRecord{
		"testPort:name1+name2+test-all": {
			Name: "testPort",
			Tags: map[string]string{
				"source": ctx.endpoint.host,
				"name":   "name1+name2+test-all",
			},
			Fields: map[string]any{
				"prop1": "value11,value21",
				"prop2": "value12;value22",
			},
		},
	}

	assertRecords(t, expected, actual)
}

func TestGather(t *testing.T) {
	acc := new(testutil.Accumulator)
	plugin := Netconf{
		Period:    "1m",
		Addresses: []string{"10.0.0.1:830"},
		Parallel:  1,
		Retries:   3,
		Subtrees: []Subtree{
			{Name: "devm", Subtree: devmSubtree, Path: "/data/devm/physical-entitys"},
			{Name: "ifm", Subtree: ifmSubtree, Path: "/data/ifm/interfaces"},
		},
		Targets: []Target{
			{
				Name:    "testPort",
				Subtree: "devm",
				Path:    "physical-entity[class='port']",
				Tags: []map[string]any{
					{"name": "./name"},
				},
				Fields: []map[string]any{
					{
						"prop0": 123,
						"_all":  true,
					},
					{
						"integer": "number(/int)",
						"boolean": "boolean(/bool = 'true')",
						"concat":  "concat(/prop1, '/', /prop2)",
					},
					{
						"_subtree": "ifm",
						"_path":    "./interface[class='main-interface' and name='{name}']",
						"prop2":    "./prop2",
						"prop3":    "./prop3",
						"prop4":    "./prop4",
						"prop5":    "./prop5",
					},
				},
			},
		},
		GetClient: newMockServer,
		Log:       Logger,
	}
	require.Nil(t, plugin.Init())

	err := plugin.Gather(acc)
	require.Nil(t, err)

	actual := map[string]TelegrafRecord{}
	for _, m := range acc.Metrics {
		actual[fmt.Sprintf("%s:%s", m.Measurement, m.Tags["name"])] = toTelegrafRecord(m)
	}

	var (
		expectedHost   = plugin.Addresses[0]
		expectedSource = strings.Split(expectedHost, ":")[0]
	)
	expected := map[string]TelegrafRecord{
		"testPort:name1": {
			Name: "testPort",
			Tags: map[string]string{
				"source": expectedSource,
				"name":   "name1",
			},
			Fields: map[string]any{
				"class":   "port",
				"name":    "name1",
				"prop0":   123,
				"prop1":   "value11",
				"integer": 112,
				"bool":    "true",
				"boolean": true,
				"concat":  "value11/value12",
				"prop2":   "value12",
				"int":     "112",
				"prop3":   "value13",
				"prop4":   "value14",
			},
		},
		"testPort:name2": {
			Name: "testPort",
			Tags: map[string]string{
				"source": expectedSource,
				"name":   "name2",
			},
			Fields: map[string]any{
				"class":   "port",
				"name":    "name2",
				"prop0":   123,
				"prop1":   "value21",
				"prop2":   "value22_new",
				"bool":    "false",
				"boolean": false,
				"concat":  "value21/value22",
				"prop5":   "value25",
			},
		},
		"testPort:test-all": {
			Name: "testPort",
			Tags: map[string]string{
				"source": expectedSource,
				"name":   "test-all",
			},
			Fields: map[string]any{
				"class":   "port",
				"name":    "test-all",
				"prop0":   123,
				"concat":  "/",
				"boolean": false,
			},
		},
	}
	assertRecords(t, expected, actual)
}

func TestRetryingSucceeds(t *testing.T) {
	acc := new(testutil.Accumulator)
	plugin := Netconf{
		Period:    "1m",
		Addresses: []string{"10.0.0.1:830"},
		Parallel:  1,
		Retries:   3,
		Subtrees: []Subtree{
			{Name: "devm", Subtree: devmSubtree, Path: "/data/devm/physical-entitys"},
			{Name: "ifm", Subtree: ifmSubtree, Path: "/data/ifm/interfaces"},
		},
		Targets: []Target{
			{
				Name:    "testPort",
				Subtree: "devm",
				Path:    "physical-entity[class='port']",
				Tags: []map[string]any{
					{"name": "./name"},
				},
				Fields: []map[string]any{
					{"prop0": 123, "_all": true},
				},
			},
		},
		GetClient: newMockServer,
		Log:       Logger,
	}

	mockServer.ExecError = fmt.Errorf("subtree get request failed")
	mockServer.FailCount = 2

	require.Nil(t, plugin.Init())
	err := plugin.Gather(acc)
	require.Nil(t, err)
	require.Equal(t, 3, len(acc.Metrics))
}

func TestRetryingFails(t *testing.T) {
	acc := new(testutil.Accumulator)
	plugin := Netconf{
		Period:    "1m",
		Addresses: []string{"10.0.0.1:830"},
		Parallel:  1,
		Retries:   3,
		Subtrees: []Subtree{
			{Name: "devm", Subtree: devmSubtree, Path: "/data/devm/physical-entitys"},
			{Name: "ifm", Subtree: ifmSubtree, Path: "/data/ifm/interfaces"},
		},
		Targets: []Target{
			{
				Name:    "testPort",
				Subtree: "devm",
				Path:    "physical-entity[class='port']",
				Tags: []map[string]any{
					{"name": "./name"},
				},
				Fields: []map[string]any{
					{"prop0": 123, "_all": true},
				},
			},
		},
		GetClient: newMockServer,
		Log:       Logger,
	}

	mockServer.ExecError = &netconf.RPCErrors{netconf.RPCError{Message: "subtree get request failed"}}
	mockServer.FailCount = 4

	err := plugin.Gather(acc)
	require.Nil(t, err)
	require.Equal(t, 0, len(acc.Metrics))
}

func TestFailAndSkipGather(t *testing.T) {
	acc := new(testutil.Accumulator)
	plugin := Netconf{
		Period:    "1m",
		Addresses: []string{"10.0.0.1:830"},
		Parallel:  1,
		Retries:   2,
		Subtrees: []Subtree{
			{
				Name:    "ifm",
				Subtree: ifmSubtree,
				Path:    "/data/ifm/interfaces",
				Skip:    true,
			},
			{
				Name:    "devm",
				Subtree: devmSubtree,
				Path:    "/data/devm/physical-entitys",
			},
		},
		Targets: []Target{
			{
				Name:    "testPort",
				Subtree: "devm",
				Path:    "physical-entity[class='port']",
				Tags: []map[string]any{
					{"name": "./name"},
				},
				Fields: []map[string]any{
					{
						"prop0": 123,
						"_all":  true,
					},
					{
						"_subtree": "ifm",
						"_path":    "./interface[class='main-interface' and name='{name}']",
						"prop2":    "./prop2",
						"prop3":    "./prop3",
						"prop4":    "./prop4",
						"prop5":    "./prop5",
					},
				},
			},
			{
				Name:    "testIfm",
				Subtree: "ifm",
				Path:    "interface[class='main-interface']",
				Tags: []map[string]any{
					{"name": "./name"},
				},
				Fields: []map[string]any{
					{
						"prop2": "./prop2",
						"prop3": "./prop3",
						"prop4": "./prop4",
						"prop5": "./prop5",
					},
				},
			},
		},
		GetClient: newMockServer,
		Log:       Logger,
	}
	mockServer.ExecError = fmt.Errorf("subtree get request failed")
	mockServer.FailCount = 1

	require.Nil(t, plugin.Init())
	err := plugin.Gather(acc)
	require.Nil(t, err)
	require.Equal(t, 3, len(acc.Metrics))
}

func TestPluginConfigAndInit(t *testing.T) {
	c := config.NewConfig()
	require.NoError(t, c.LoadConfig("sample.conf"))
	require.Len(t, c.Inputs, 1)
	plugin, ok := c.Inputs[0].Input.(*Netconf)
	require.True(t, ok)
	require.NoError(t, plugin.Init())

	require.Len(t, plugin.Targets, 5)
	require.Equal(t, "metric-1", plugin.Targets[0].Name)
	require.Equal(t, "metric-2", plugin.Targets[1].Name)
	require.Equal(t, "metric-3", plugin.Targets[2].Name)
	require.Equal(t, "metric-4", plugin.Targets[3].Name)
	require.Equal(t, "metric-5", plugin.Targets[4].Name)
}
