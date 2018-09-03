Nginx New Relic Plugin
----------------------

This is a re-implementation of the official [NGiNX New Relic plugin][1] in Go so
that it compiles down to a static binary with no Python runtime required. It
only grabs the stats that are including the [http-stub-status][2] module and does
not add all the stats available from NGiNX Plus.

Aside from being just a static binary, it's also a 12-factor app which is good
for running in containers:

* All configuration is from environment variables
* Logging is to stdout

On startup the plugin prints its configuration to the log so you can see how
it ran.

Example Config
--------------

```
AGENT_NEW_RELIC_APP_NAME="NGiNX Gateway Foo" \
AGENT_STATS_URL=http://localhost:32768/health \
AGENT_NEW_RELIC_LICENSE_KEY="<your license key here>" \
./nginx-nr-agent
```

You can also change the New Relic API URL used like this:

```
AGENT_NEW_RELIC_API_URL="<your url here>"
```

Generally this is not a setting you'll need to change.

[1]: https://github.com/skyzyx/nginx-nr-agent
[2]: http://nginx.org/en/docs/http/ngx_http_stub_status_module.html
