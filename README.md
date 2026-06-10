# Hardware-Aware-Push-CDN

. Executive Summary
A distributed content delivery network optimized for ultra-low latency live video streaming. The system bypasses standard HTTP polling by utilizing file-system event watchers to actively push video chunks (.ts) and playlists (.m3u8) from Origin to Edge nodes the millisecond they are generated. Traffic routing is dictated by a custom Load Balancer that monitors real-time hardware telemetry (CPU, RAM, Network I/O) from Edge nodes to ensure zero frame-drops under heavy load.

2. Module Specifications
Module 1: Push Origin (ngx_http_push_origin)
Primary Role: Watch the file system for newly encoded video files and aggressively push them to registered downstream Edge servers.

Core Triggers: * Listens on a dedicated socket for Edge nodes to register via the custom OEP (Origin Edge Protocol).

Uses Linux inotify to detect IN_CLOSE_WRITE, IN_MOVED_TO, and IN_CREATE events in the designated video root directory.

Processing Logic:

Maintains a thread pool (max children) of connected Edge servers.

When inotify detects a completed file write, the module parses the file path.

If the file is a Master .m3u8, it parses the file for secondary playlist URLs.

If the file is a .ts chunk, it queues it for transfer.

Outputs / Actions:

Uses libcurl to execute an HTTP PUT request, sending the raw file payload to the put_address provided by the Edge server.

Configuration Parameters:

ons_push_origin_root_directory: The directory FFmpeg/OBS writes to.

ons_push_origin_port: Port to listen for OEP connections.

ons_push_origin_max_children: Maximum allowed Edge nodes.

ons_push_origin_net_max: Network bandwidth limits before denying new Edge nodes.

Module 2: Push Edge (ngx_http_push_edge)
Primary Role: Act as the ingest point for Origin pushes and the egress point for end-user video playback.

Core Triggers:

End-user HTTP GET requests for a specific video stream directory.

Origin server HTTP PUT requests containing video files.

Processing Logic:

When a user requests a stream, the Edge checks if it is currently subscribed to that stream.

If not, it opens a socket connection to the Origin and sends an OEP_JOIN command containing its own HTTP PUT address and the requested stream name.

Maintains a timeout queue. If no users request a stream for a defined period (e.g., 300 seconds), it sends an OEP_LEAVE command to the Origin to stop receiving pushes for that stream to save bandwidth.

Outputs / Actions:

Writes received PUT payloads to the local disk/memory.

Serves local files to the requesting users.

Module 3: Session Manager / Hardware Monitor (ngx_http_session_manager)
Primary Role: Generate unique viewer session IDs and continuously monitor the host machine's hardware saturation.

Core Triggers:

Intercepts incoming HTTP requests to append a ?sid= UUID parameter for tracking.

A recurring timer event (ping period) triggers the hardware audit.

Processing Logic:

CPU: Parses /proc/stat to calculate usage percentages (totalUser, totalSys, totalIdle).

RAM/Swap: Calls sysinfo() to calculate used vs. total memory.

Disk: Calls statvfs() on the root directory to check capacity.

Network: Parses /sys/class/net/{interface}/statistics/tx_bytes to measure outbound traffic against the server's known link speed.

Calculates a unified "Hardware Score" aggregating these metrics (weighted towards CPU and Network).

Outputs / Actions:

Generates a JSON diagnostic string at ons_session_manager_json for observability.

Opens a TCP socket to the Load Balancer and sends the hardware score via the custom LBP (Load Balancer Protocol).

Module 4: Load Balancer (ngx_http_loadbalancer)
Primary Role: Route new user sessions to the healthiest available Edge node based on real-time hardware telemetry.

Core Triggers:

Listens for LBP socket connections from Edge node Session Managers.

End-user HTTP requests hitting the main entry point.

Processing Logic:

Maintains a shared memory queue of all active Edge peers.

Parses incoming LBP reports to update the cpu, ram, swap, tx, disk, and sess metrics for each peer.

Circuit Breaker: If a peer exceeds predefined thresholds (e.g., CPU > 80%), it flags the peer as down and temporarily removes it from the routing pool.

Routing Policies:

LL (Least Loaded): Scans the queue and selects the peer with the lowest overall hardware score.

RR (Round Robin): Cycles sequentially through healthy peers.

Outputs / Actions:

Resolves the variable $ons_loadbalancer_result with the IP address of the chosen Edge server, allowing NGINX to issue an HTTP 301/302 Redirect.

Module 5: Stream Push State Manager (ngx_http_stream_push)
Primary Role: Distributed state management using Redis Pub/Sub to coordinate stream availability across multiple clusters.

Core Triggers:

User requests a stream file that is currently missing from the Edge's local disk.

Processing Logic:

Pauses the user's HTTP request and connects to the Redis Cluster.

Uses PUBLISH {topic} {stream_directory}|Join to broadcast that this Edge needs the stream.

Initiates an event loop to poll Redis (e.g., GET OSPP:{stream_directory}) to check if the stream state has become ACTIVE (meaning the Origin has started pushing it).

Outputs / Actions:

Once the file arrives, it resumes the HTTP request and serves the file.

If the file never arrives (timeout), it returns an HTTP 504 Gateway Timeout.

3. Data Contracts & Custom Protocols
(Note for Go Rewrite: These custom protocols should be entirely replaced by gRPC/Protobufs or secure HTTP/JSON payloads, but this describes their current mechanical format).

Origin Edge Protocol (OEP)
Used by the Edge to tell the Origin to start or stop pushing a stream.

Format: [Signature (3)][Command (1)][Req_Len (3)][Request_Dir (N)][Put_Len (3)][Put_Address (N)]

Example: OEP1005/temp022http://172.16.23.8/put

OEP: Signature

1: Command (1 = JOIN, 2 = LEAVE)

005/temp: Length and Name of stream directory

022http://...: Length and URL of where the Origin should HTTP PUT the files.

Load Balancer Protocol (LBP)
Used by the Session Manager to report hardware telemetry to the Load Balancer.

Register Format: [Signature (3)][Command (1)][Length (2)][Payload (N)]

Command 1 = Register, 3 = Self-Register.

Report Format: [Signature (3)][Command (1)][Delimiter (1)][CPU (3)][RAM (3)][SWAP (3)][TX (3)][DISK (3)][SESS (3)]

Example: LBP21002073003000050010

LBP: Signature

2: Command (REPORT)

1: Delimiter

Values are packed as 3-digit percentages/metrics.
