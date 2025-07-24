package netconf

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"strings"
	"time"

	"github.com/antchfx/xmlquery"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/networkguild/netconf"
	ncssh "github.com/networkguild/netconf/transport/ssh"
	"golang.org/x/crypto/ssh"
)

type Connection interface {
	Connect(context.Context, string, string, config.Secret) error
	Close() error
	SetLogger(telegraf.Logger)
	GetSubtrees(context.Context, int, []Subtree) (map[string]*xmlquery.Node, error)
	GetSubtree(context.Context, Subtree) (*xmlquery.Node, error)
	SessionID() uint64
}

// Client holds the state of a Netconf connection
type Client struct {
	session netconf.ISession
	log     telegraf.Logger
}

func (client *Client) SetLogger(log telegraf.Logger) {
	client.log = log
}

func (client *Client) GetSubtrees(ctx context.Context, retries int, subtrees []Subtree) (map[string]*xmlquery.Node, error) {
	start := time.Now()
	collected := make(map[string]*xmlquery.Node, len(subtrees))
	for _, subtree := range subtrees {
		name := subtree.Name
		node, skipped, err := getWithRetry(ctx, subtree, retries, client.GetSubtree)
		switch {
		case skipped:
			client.log.Warnf("Failed to collect subtree %s, error: %v, skipping subtree", name, err)
		case err != nil:
			return collected, fmt.Errorf("failed to collect subtree %s, error: %v", name, err)
		default:
			client.log.Debugf("Collected subtree %s", name)
			collected[name] = node
		}
	}
	client.log.Infof("Collected %d subtrees in %s.", len(subtrees), time.Now().Sub(start))
	return collected, nil
}

func getWithRetry(ctx context.Context, subtree Subtree, retries int, get func(context.Context, Subtree) (*xmlquery.Node, error)) (*xmlquery.Node, bool, error) {
	var err error
	for i := 0; i <= retries; i++ {
		var node *xmlquery.Node
		node, err = get(ctx, subtree)
		switch {
		case node != nil:
			return node, false, nil
		case err != nil && subtree.Skip:
			return nil, true, err
		default:
			time.Sleep(retryWait)
		}
	}
	return nil, false, err
}

// Connect sets up a Netconf connection
func (client *Client) Connect(ctx context.Context, address, username string, password config.Secret) error {
	passwordSecret, err := password.Get()
	if err != nil {
		return err
	}
	sshConfig := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.Password(passwordSecret.String()),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	passwordSecret.Destroy()

	tr, err := ncssh.Dial(ctx, "tcp", address, sshConfig)
	if err != nil {
		return err
	}

	session, err := netconf.NewSession(ctx, netconf.WithTransport(tr))
	if err != nil {
		return err
	}

	client.session = session
	return nil
}

// Close closes a Netconf connection
func (client *Client) Close() error {
	defer func() {
		client.session = nil
	}()
	return client.session.Close(context.Background())
}

func (client *Client) SessionID() uint64 {
	return client.session.SessionID()
}

func (client *Client) GetSubtree(ctx context.Context, subtree Subtree) (*xmlquery.Node, error) {
	var (
		reply *netconf.RPCReply
		err   error
	)
	switch subtree.Method {
	case "", "get":
		reply, err = client.session.Get(ctx,
			netconf.WithSubtreeFilter(subtree.Subtree),
			netconf.WithDefaultMode(netconf.DefaultsMode(subtree.WithDefaults)))
	case "get-config":
		datastore := netconf.Running
		if subtree.Datastore != "" {
			datastore = netconf.Datastore{Store: subtree.Datastore}
		}
		reply, err = client.session.GetConfig(ctx,
			datastore,
			netconf.WithSubtreeFilter(subtree.Subtree),
			netconf.WithDefaultMode(netconf.DefaultsMode(subtree.WithDefaults)))
	case "rpc":
		reply, err = client.session.Dispatch(ctx, subtree.Subtree)
	}
	if err != nil {
		var (
			rpcErrors netconf.RPCErrors
			rpcError  netconf.RPCError
			reply     = &netconf.RPCReply{
				XMLName: xml.Name{Local: "rpc-reply", Space: "urn:ietf:params:xml:ns:netconf:base:1.0"},
			}
		)

		if errors.As(err, &rpcErrors) {
			reply.Errors = rpcErrors
		} else if errors.As(err, &rpcError) {
			reply.Errors = netconf.RPCErrors{rpcError}
		}

		if reply.Errors != nil {
			rpcReply, _ := xml.MarshalIndent(reply, "", "  ")
			unescaped := html.UnescapeString(string(rpcReply))
			client.log.Warnf("Subtree %s collection failed with rpc error:\n%s", subtree.Name, unescaped)
		} else {
			client.log.Errorf("Command execution failed: %v", err)
		}

		return nil, err
	} else if reply == nil {
		return nil, fmt.Errorf("no reply received for command: %s", subtree.Subtree)
	}

	if client.log.Level() == telegraf.Trace {
		client.log.Tracef("Reply:\n%s", bytes.TrimSpace(reply.Raw()))
	}

	return parseNodeFromData(subtree.Path, reply.Raw(), subtree.RemoveNewlines)
}

func parseNodeFromData(xpath string, data []byte, remove bool) (*xmlquery.Node, error) {
	var reader io.Reader
	if remove {
		reader = bytes.NewReader(bytes.ReplaceAll(data, []byte("\n"), nil))
	} else {
		reader = bytes.NewReader(data)
	}
	root, err := xmlquery.ParseWithOptions(reader, xmlquery.ParserOptions{
		Decoder: &xmlquery.DecoderOptions{
			Strict: false,
		},
	})

	if err != nil || root == nil {
		return nil, fmt.Errorf("failed to parse reply: %v", err)
	}

	if xpath == "" {
		return root, nil
	}

	if strings.HasPrefix(xpath, "/data") {
		xpath = "/rpc-reply" + xpath
	}
	node := xmlquery.FindOne(root, xpath)
	if node == nil {
		return nil, fmt.Errorf("nothing found at %s", xpath)
	} else {
		return node, nil
	}
}
