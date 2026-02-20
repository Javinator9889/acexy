# `acexy` - An AceStream Proxy Written In Go! ⚡

[![Go Build](https://github.com/Javinator9889/acexy/actions/workflows/build.yaml/badge.svg)](https://github.com/Javinator9889/acexy/actions/workflows/build.yaml)
[![Docker Release](https://github.com/Javinator9889/acexy/actions/workflows/release.yaml/badge.svg?event=release)](https://github.com/Javinator9889/acexy/actions/workflows/release.yaml)

## Table of Contents

- [How It Works? 🛠](#how-it-works-)
- [Key Features 🔗](#key-features-)
- [Usage 📐](#usage-)
- [Dynamic Orchestration 🐳](#dynamic-orchestration-)
- [Optimizing 🚀](#optimizing-)
  - [Alternative 🧃](#alternative-)
- [Configuration Options ⚙](#configuration-options-)

## How It Works? 🛠

This project is a wrapper around the
[AceStream middleware HTTP API](https://docs.acestream.net/developers/start-playback/#using-middleware), allowing both
[HLS](https://en.wikipedia.org/wiki/HTTP_Live_Streaming) and
[MPEG-TS](https://en.wikipedia.org/wiki/HTTP_Live_Streaming) playback
of a stream.

I was tired of the limitations of AceStream and some of the problems that 
exist when playing a stream 📽. For example, it is only possible to play
the same channel for **1 single client**. For having multiple clients
playing **different streams**, you must manually add a unique `pid` per 
client. If there was an error during the transmission, the **whole stream
goes down**, etc.

I found quite frustrating the experience of using AceStream in a home network
with a single server and multiple clients, to try to optimize resources. This
is the topology for which I am using AceStream:

![AceStream Topology For My Network](doc/img/topology.svg)

There are some problems:

* Only **one client** can play the same stream at a time 🚫.
* Having each client to run AceStream on their own is a waste of resources
  and saturates the network 📉.
* Multiple clients can play different streams if they have a unique `pid`
  (Player ID) associated 🔓.
* The standard AceStream HTTP API is not resilient enough against errors,
  if the transmission stops it stops for every client ❌.

## Key Features 🔗

When using `acexy`, you automatically have:

* A single, centralized server running **all your AceStream streams** ⛓.
* Automatic assignation of a unique `pid` (Player ID) **per client per stream** 🪪.
* **Stream Multiplexing** 🕎: The same stream can be reproduced *at the
  same time in multiple clients*.
* **Resilient, error-proof** streaming thanks to the HTTP Middleware 🛡.
* **Dynamic AceStream Orchestration** 🐳: Automatically spin up and tear down AceStream
  containers on demand — no manual management required.
* **Intelligent Load Balancing** ⚖️: Streams are distributed across instances based on
  current load, respecting a configurable maximum of concurrent streams per instance.
* **Auto Scale-Down** 📉: Idle instances are automatically removed after a configurable
  timeout, keeping resource usage minimal.
* **Graceful Shutdown** 🛑: All dynamically created containers are cleaned up automatically
  when acexy stops.
* *Blazing fast, minimal proxy* ☄ written in Go!

With this proxy, the following architecture is now possible:

![acexy Topology](doc/img/acexy.svg)

## Usage 📐

`acexy` is available as a Docker image. Make sure you have the latest
[Docker](https://docker.com) installed and available.

The recommended setup uses Docker Compose, which handles acexy, the Docker
socket proxy, and the dynamic AceStream instances automatically:

```shell
wget https://raw.githubusercontent.com/Javinator9889/acexy/refs/heads/main/docker-compose.yml
docker compose up -d
```

There is a single available endpoint: `/ace/getstream` which takes the same
parameters as the standard
[AceStream Middleware/HTTP API](https://docs.acestream.net/developers/api-reference/). Therefore,
for running a stream, just open the following link in your preferred application - such as VLC:

```
http://127.0.0.1:8080/ace/getstream?id=dd1e67078381739d14beca697356ab76d49d1a2
```

where `dd1e67078381739d14beca697356ab76d49d1a2` is the ID of the AceStream channel.

You can also check the status of active streams at any time:

```
http://127.0.0.1:8080/ace/status
```

By default, the proxy will work in MPEG-TS mode. For switching between them,
you must add the **`-m3u8` flag** or set **`ACEXY_M3U8=true` environment variable**.

> **NOTE**: The HLS mode - `ACEXY_M3U8` or `-m3u8` flag - is in a non-tested
> status. Using it is discouraged and not guaranteed to work.

## Dynamic Orchestration 🐳

acexy includes a built-in orchestrator that manages AceStream containers automatically.
There is **no need to run or manage AceStream manually** — acexy handles everything.

**How it works:**

- On startup, acexy creates `ACESTREAM_MIN_REPLICAS` AceStream containers and waits for
  them to be healthy before accepting requests.
- When a new stream is requested and all existing instances are at capacity
  (`ACESTREAM_STREAMS_PER_INSTANCE`), acexy automatically spins up a new instance
  (up to `ACESTREAM_MAX_REPLICAS`).
- Instances with no active streams are automatically removed after `ACESTREAM_IDLE_TIMEOUT`
  seconds, as long as the number of instances stays above `ACESTREAM_MIN_REPLICAS`.
- On shutdown (`SIGTERM` / `SIGINT`), all dynamically created containers are removed cleanly.

**Requirements:**

The orchestrator communicates with Docker via a socket proxy for security. The
`docker-compose.yml` includes the `tecnativa/docker-socket-proxy` service preconfigured.
You should never mount the Docker socket directly into acexy.

**VPN support:**

Set `COMPOSE_PROFILE=vpn` to route AceStream traffic through a
[Gluetun](https://github.com/qdm12/gluetun) container. Uncomment the `gluetun` service
in `docker-compose.yml` and configure your VPN provider. In this mode, AceStream
instances use Gluetun's network namespace instead of the shared bridge network.

## Configuration Options ⚙

Acexy has tons of configuration options that allow you to customize the behavior. All of them have
default values that were tested for the optimal experience, but you may need to adjust them
to fit your needs.

> **PRO-TIP**: You can issue `acexy -help` to have a complete view of all the available options.

As Acexy was thought to be run inside a Docker container, all the variables and settings are
adjustable by using environment variables.


Acexy has tons of configuration options. All of them have default values tested for optimal
experience, but you may need to adjust them to fit your needs.

> **PRO-TIP**: You can issue `acexy -help` to have a complete view of all the available options.

### Proxy Options

<table>
  <thead>
    <tr>
      <th>Flag</th>
      <th>Environment Variable</th>
      <th>Description</th>
      <th>Default</th>
    </tr>
  </thead>
  <tbody>
    <tr>
      <th><code>-license</code></th>
      <th>-</th>
      <th>Prints the program license and exits</th>
      <th>-</th>
    </tr>
    <tr>
      <th><code>-help</code></th>
      <th>-</th>
      <th>Prints the help message and exits</th>
      <th>-</th>
    </tr>
    <tr>
      <th><code>-addr</code></th>
      <th><code>ACEXY_LISTEN_ADDR</code></th>
      <th>Address where Acexy is listening to.</th>
      <th><code>:8080</code></th>
    </tr>
    <tr>
      <th><code>-scheme</code></th>
      <th><code>ACEXY_SCHEME</code></th>
      <th>The scheme of the AceStream middleware.</th>
      <th><code>http</code></th>
    </tr>
    <tr>
      <th><code>-acestream-host</code></th>
      <th><code>ACEXY_HOST</code></th>
      <th>Fallback AceStream host (used when orchestration is disabled).</th>
      <th><code>localhost</code></th>
    </tr>
    <tr>
      <th><code>-acestream-port</code></th>
      <th><code>ACEXY_PORT</code></th>
      <th>Fallback AceStream port (used when orchestration is disabled).</th>
      <th><code>6878</code></th>
    </tr>
    <tr>
      <th><code>-m3u8-stream-timeout</code></th>
      <th><code>ACEXY_M3U8_STREAM_TIMEOUT</code></th>
      <th>When running in M3U8 mode, the timeout to consider a stream is done.</th>
      <th><code>60s</code></th>
    </tr>
    <tr>
      <th><code>-m3u8</code></th>
      <th><code>ACEXY_M3U8</code></th>
      <th>Enable M3U8 mode. <b>WARNING</b>: Experimental, may not work as expected.</th>
      <th>Disabled</th>
    </tr>
    <tr>
      <th><code>-empty-timeout</code></th>
      <th><code>ACEXY_EMPTY_TIMEOUT</code></th>
      <th>Time without receiving stream data after which the stream is considered stalled and a reconnect is attempted.</th>
      <th><code>30s</code></th>
    </tr>
    <tr>
      <th><code>-empty-retry-count</code></th>
      <th><code>ACEXY_EMPTY_RETRY_COUNT</code></th>
      <th>Number of reconnect attempts when a stream stalls before giving up and closing it. Set to <code>0</code> to disable retries.</th>
      <th><code>3</code></th>
    </tr>
    <tr>
      <th><code>-buffer-size</code></th>
      <th><code>ACEXY_BUFFER_SIZE</code></th>
      <th>Buffer size before copying stream data to players. Increase for better stability.</th>
      <th><code>4.2MiB</code></th>
    </tr>
    <tr>
      <th><code>-no-response-timeout</code></th>
      <th><code>ACEXY_NO_RESPONSE_TIMEOUT</code></th>
      <th>Time to wait for the AceStream middleware to respond to a new stream request.</th>
      <th><code>1s</code></th>
    </tr>
  </tbody>
</table>

### Orchestrator Options

<table>
  <thead>
    <tr>
      <th>Flag</th>
      <th>Environment Variable</th>
      <th>Description</th>
      <th>Default</th>
    </tr>
  </thead>
  <tbody>
    <tr>
      <th><code>-min-replicas</code></th>
      <th><code>ACESTREAM_MIN_REPLICAS</code></th>
      <th>Minimum number of AceStream instances to keep running at all times.</th>
      <th><code>1</code></th>
    </tr>
    <tr>
      <th><code>-max-replicas</code></th>
      <th><code>ACESTREAM_MAX_REPLICAS</code></th>
      <th>Maximum number of AceStream instances allowed.</th>
      <th><code>5</code></th>
    </tr>
    <tr>
      <th><code>-streams-per-instance</code></th>
      <th><code>ACESTREAM_STREAMS_PER_INSTANCE</code></th>
      <th>Maximum number of concurrent streams per AceStream instance before scaling up.</th>
      <th><code>3</code></th>
    </tr>
    <tr>
      <th><code>-idle-timeout</code></th>
      <th><code>ACESTREAM_IDLE_TIMEOUT</code></th>
      <th>Time after which an idle instance (no active streams) is automatically removed.</th>
      <th><code>5m</code></th>
    </tr>
    <tr>
      <th><code>-recycle-timeout</code></th>
      <th><code>ACESTREAM_RECYCLE_TIMEOUT</code></th>
      <th>Idle time after which the entire pool is replaced with fresh instances. Set to <code>0</code> to disable.</th>
      <th><code>60s</code></th>
    </tr>
    <tr>
      <th><code>-recycle-check-interval</code></th>
      <th><code>ACESTREAM_RECYCLE_CHECK_INTERVAL</code></th>
      <th>How often the recycle check runs. Lower values make the recycle more precise at the cost of slightly more CPU.</th>
      <th><code>3s</code></th>
    </tr>
    <tr>
      <th><code>-scale-down-interval</code></th>
      <th><code>ACESTREAM_SCALE_DOWN_INTERVAL</code></th>
      <th>How often the scale down check runs. This check may call the Docker API, so keep it above a few seconds.</th>
      <th><code>15s</code></th>
    </tr>
    <tr>
      <th><code>-acestream-image</code></th>
      <th><code>ACESTREAM_IMAGE</code></th>
      <th>Docker image to use for AceStream instances.</th>
      <th><code>martinbjeldbak/acestream-http-proxy:latest</code></th>
    </tr>
    <tr>
      <th><code>-compose-profile</code></th>
      <th><code>COMPOSE_PROFILE</code></th>
      <th>Network profile for AceStream containers. Use <code>regular</code> for bridge network or <code>vpn</code> to route through Gluetun.</th>
      <th><code>regular</code></th>
    </tr>
    <tr>
      <th><code>-docker-host</code></th>
      <th><code>DOCKER_HOST</code></th>
      <th>Docker host URL. Should point to a Docker socket proxy, never mount the socket directly.</th>
      <th><code>tcp://docker-proxy:2375</code></th>
    </tr>
    <tr>
      <th><code>-container-failure-threshold</code></th>
      <th><code>ACESTREAM_CONTAINER_FAILURE_THRESHOLD</code></th>
      <th>Consecutive container health check failures before marking an instance as unhealthy and replacing it.</th>
      <th><code>3</code></th>
    </tr>
    <tr>
      <th><code>-stream-failure-threshold</code></th>
      <th><code>ACESTREAM_STREAM_FAILURE_THRESHOLD</code></th>
      <th>Consecutive times all active streams in an instance stall before marking it as unhealthy and replacing it.</th>
      <th><code>3</code></th>
    </tr>
  </tbody>
</table>

> **NOTE**: The list of options is extensive but could be outdated. Always refer to the
> Acexy binary `-help` output when in doubt.
