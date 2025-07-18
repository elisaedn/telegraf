# Netconf Input Plugin

This plugin collect configuration data using [netconf][netconf].

Use **log_level**: `trace` to log xml requests.

⭐ Telegraf v1.6.0
🏷️ network
💻 all

[netconf]: https://www.rfc-editor.org/rfc/rfc6241.html

## Global configuration options <!-- @/docs/includes/plugin_config.md -->

In addition to the plugin-specific configuration settings, plugins support
additional global and plugin configuration settings. These settings are used to
modify metrics, tags, and field or create aliases and configure ordering, etc.
See the [CONFIGURATION.md][CONFIGURATION.md] for more details.

[CONFIGURATION.md]: ../../../docs/CONFIGURATION.md#plugins

## Secret-store support

This plugin supports secrets from secret-stores for the `username` and
`password` options. See the [secret-store documentation][SECRETSTORE] for more
details on how to use them.

[SECRETSTORE]: ../../../docs/CONFIGURATION.md#secret-store-secrets

## Configuration

```toml @sample.conf
[[inputs.netconf]]
  ## List of collection endpoint
  # Host can be a numeric IP address or a host name.
  addresses = ["10.0.0.1:830", "host.name:22", "10.0.0.3:830"]
  username = "user"
  password = "pass"
  # Max number of endpoints to process in parallel
  endpoints_in_parallel = 1
  # Number of retries for requesting a subtree
  retries = 3

## Subtrees to get from each endpoint
# Subtrees have a name (used in targets to refer to a particular subtree) and an XML subtree to be
# used in an RPC get command `<get><filter type="subtree">{SUBTREE}</filter></get>`
# Subtrees have also a path (relative to /rpc-reply) to the reference element for targets.
[[inputs.netconf.subtree]]
  name = "subtree-name"
  subtree = "xml subtree for netconf rpc get command"
  path = "/rpc-reply/data/some-element"

[[inputs.netconf.subtree]]
  name = "another-subtree"
  subtree = "XML subtree for netconf rpc get command"
  with_defaults = "report-all" # Optional with-defaults directive to add to the query, e.g. report-all, trim, explicit, report-all-tagged
  path = "/rpc-reply/data"
  datastore = "running" # Optional, default is "candidate, only used with get-config"
  rpc_method = "get" # Optional, default is "get", get|get-config|rpc
  skip = false # boolean value if subtree should skipped after failure
  remove_newlines = false # If true, removes all newlines from rpc-reply

## Targets to collect from the subtrees
# Targets have name (used as the metric name), subtree name and path from the
# named subtree's reference element to target element(s).
# Targets also have tables defining tags and fields for the target metric.
# Each table may have its own subtree name (as "_subtree") and path (as "_path")
# to refer to other elements than the target elements. The path in "_path" is relative
# to the reference element of the subtree specified in "_subtree", or if there is no
# "_subtree" for the table, to the target element.
# If table has "_all" item set to true all text elements of table's reference element
# and its descendants are collected.
# Each value in the tag and field tables are either plain value, used as-is, or a path
# to element with text content. The text content will be used as the actual value.
# Such paths must start with "./" and are relative the table's reference element.
# Values can also contain references to tag values as "{tag-name}".  Such references
# are replaced with the corresponding tag value.  The tag value must have been
# determined in a preceeding table.

[[inputs.netconf.target]]
  name = "metric-1"
  subtree = "subtree-name"
  path = "./relative/path/to/target"

  [[inputs.netconf.target.tags]]
    tag_name_1 = "tag-value"
    tag_name_2 = 123
    tag_name_3 = "./path/to/element/with/text"

  [[inputs.netconf.target.fields]]
    field_name-1 = "field-value"
    field_name-2 = "{tag-name-1}"

[[inputs.netconf.target]]
  name = "metric-2"
  subtree = "subtree-name"
  path = "./relative/path/to/target"

[[inputs.netconf.target.fields]]
  _all = true  # all text elements of target element and descendants are collected as fields

[[inputs.netconf.target.fields]]
  _subtree = "another-subtree"           # a table can refer to different subtree than the target
  _path = "./path/to/table/element"      # path to table element(s) within the other subtree
  field_name = "./path/relative/to/table/element"

[[inputs.netconf.target]]
  name = "metric-3"

[[inputs.netconf.target]]
  name = "metric-4"

[[inputs.netconf.target]]
  name = "metric-5"
```
