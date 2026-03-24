"""
Minimal fake AirPlay receiver for integration testing.

Wraps pyatv's AirPlayServerAuth to handle HAP pair-setup, pair-verify, and
FairPlay fp-setup, then adds the MVP control endpoints (/info, /play,
/playback-info, /rate, /stop, /scrub, /reverse).

Outputs newline-delimited JSON events to stdout so that the Go test harness
can parse them. Advertises itself via mDNS (_airplay._tcp) using zeroconf.

Usage:
  uv run python fake_receiver.py [--port PORT] [--name NAME] [--interface IFACE]

Output (stdout, one JSON object per line):
  {"type":"ready","port":PORT,"pk":"HEXPK"}         -- server is up and advertised
  {"type":"play","url":"...","position":0.0}         -- /play received
  {"type":"rate","value":1.0}                        -- /rate received
  {"type":"scrub","position":30.5}                   -- /scrub received
  {"type":"stop"}                                    -- /stop received
"""

import argparse
import asyncio
import json
import plistlib
import socket
import sys

from pyatv.auth.hap_session import HAPSession
from pyatv.auth.server_auth import PRIVATE_KEY, SERVER_IDENTIFIER
from pyatv.protocols.airplay.server_auth import AirPlayServerAuth, generate_keys
from pyatv.support import http
from pyatv.support.http import BasicHttpServer, HttpResponse, http_server
from zeroconf.asyncio import AsyncServiceInfo, AsyncZeroconf


def emit(event: dict):
    """Write a JSON event to stdout and flush immediately."""
    print(json.dumps(event), flush=True)


class FakeReceiver(BasicHttpServer, AirPlayServerAuth):
    """Fake AirPlay receiver combining pyatv's auth with MVP control endpoints."""

    def __init__(self):
        AirPlayServerAuth.__init__(self, "FoxCast-Test")
        BasicHttpServer.__init__(self, self)
        self._hap_session: HAPSession | None = None
        self._encryption_active = False
        self._playback_state = {"rate": 0.0, "position": 0.0, "duration": 0.0}

        self.add_route("GET", "^/info$", self.handle_info_override)
        self.add_route("POST", "^/play$", self.handle_play)
        self.add_route("GET", "^/playback-info$", self.handle_playback_info)
        self.add_route("POST", "^/rate$", self.handle_rate)
        self.add_route("POST", "^/scrub$", self.handle_scrub)
        self.add_route("POST", "^/stop$", self.handle_stop)
        self.add_route("POST", "^/reverse$", self.handle_reverse)

    # --- HAP encryption wiring ---

    def enable_encryption(self, output_key: bytes, input_key: bytes) -> None:
        self._hap_session = HAPSession()
        self._hap_session.enable(output_key, input_key)

    def process_received(self, data: bytes) -> bytes:
        if self._hap_session:
            if not self._encryption_active:
                self._encryption_active = True
            data = self._hap_session.decrypt(data)
        return data

    def process_sent(self, data: bytes) -> bytes:
        if self._encryption_active and self._hap_session:
            data = self._hap_session.encrypt(data)
        return data

    # --- Helper ---

    def _ok(self, request, body=b"", content_type="application/x-apple-binary-plist"):
        headers = {"CSeq": request.headers.get("CSeq", "1")}
        if body:
            headers["Content-Type"] = content_type
        return HttpResponse(request.protocol, request.version, 200, "OK", headers, body)

    # --- Control endpoint handlers ---

    def handle_info_override(self, request):
        keys = generate_keys(PRIVATE_KEY)
        pk_hex = keys.auth_pub.hex()
        body = {
            "deviceID": "ff:ee:dd:cc:bb:aa",
            "macAddress": "ff:ee:dd:cc:bb:aa",
            "name": "FoxCast-Test",
            "model": "AppleTV6,2",
            "manufacturer": "Apple Inc.",
            "pi": SERVER_IDENTIFIER,
            "pk": keys.auth_pub,
            "features": 0x4A7FDFD538BCB46,
            "statusFlags": 4,
            "protocolVersion": "1.1",
            "sourceVersion": "550.10",
            "audioLatencies": [{"inputLatencyMicros": 0, "outputLatencyMicros": 79000}],
        }
        return self._ok(request, plistlib.dumps(body))

    def handle_play(self, request):
        body = plistlib.loads(request.body) if request.body else {}
        url = body.get("Content-Location", "")
        position = body.get("Start-Position", body.get("Start-Position-Seconds", 0.0))
        self._playback_state["rate"] = 1.0
        self._playback_state["position"] = float(position)
        emit({"type": "play", "url": url, "position": float(position)})
        return self._ok(request)

    def handle_playback_info(self, request):
        body = {
            "duration": self._playback_state["duration"],
            "position": self._playback_state["position"],
            "rate": self._playback_state["rate"],
            "readyToPlay": True,
            "playbackBufferEmpty": False,
            "playbackBufferFull": False,
            "playbackLikelyToKeepUp": True,
            "loadedTimeRanges": [{"duration": 0.0, "start": 0.0}],
            "seekableTimeRanges": [{"duration": 0.0, "start": 0.0}],
        }
        return self._ok(request, plistlib.dumps(body))

    def handle_rate(self, request):
        # value comes as query param: POST /rate?value=1.0
        path = request.path  # e.g. "/rate?value=1.0"
        value = 0.0
        if "value=" in path:
            try:
                value = float(path.split("value=")[1].split("&")[0])
            except (ValueError, IndexError):
                pass
        self._playback_state["rate"] = value
        emit({"type": "rate", "value": value})
        return self._ok(request)

    def handle_scrub(self, request):
        path = request.path
        position = 0.0
        if "position=" in path:
            try:
                position = float(path.split("position=")[1].split("&")[0])
            except (ValueError, IndexError):
                pass
        self._playback_state["position"] = position
        emit({"type": "scrub", "position": position})
        return self._ok(request)

    def handle_stop(self, request):
        self._playback_state["rate"] = 0.0
        emit({"type": "stop"})
        return self._ok(request)

    def handle_reverse(self, request):
        # Acknowledge the PTTH upgrade; we don't send events back in tests.
        return HttpResponse(
            "HTTP",
            "1.1",
            101,
            "Switching Protocols",
            {"Upgrade": "PTTH/1.0", "Connection": "Upgrade"},
            b"",
        )


async def run(port: int, name: str, interface: str):
    server, actual_port = await http_server(FakeReceiver, "0.0.0.0", port)

    keys = generate_keys(PRIVATE_KEY)
    pk_hex = keys.auth_pub.hex()

    # Advertise via mDNS
    local_ip = socket.gethostbyname(socket.gethostname()) if interface == "" else interface
    properties = {
        "deviceid": "ff:ee:dd:cc:bb:aa",
        "features": "0x4A7FDFD5,0x038BCB46",
        "flags": "0x4",
        "model": "AppleTV6,2",
        "pk": pk_hex,
        "pi": SERVER_IDENTIFIER,
        "srcvers": "550.10",
        "vv": "2",
        "acl": "0",
    }
    info = AsyncServiceInfo(
        "_airplay._tcp.local.",
        f"{name}._airplay._tcp.local.",
        addresses=[socket.inet_aton(local_ip)],
        port=actual_port,
        properties=properties,
    )
    zc = AsyncZeroconf(interfaces=[local_ip] if local_ip else None)
    await zc.async_register_service(info)

    emit({"type": "ready", "port": actual_port, "pk": pk_hex})

    try:
        await asyncio.Event().wait()
    finally:
        await zc.async_unregister_service(info)
        await zc.async_close()
        server.close()


def main():
    parser = argparse.ArgumentParser(description="Fake AirPlay receiver for testing")
    parser.add_argument("--port", type=int, default=0, help="Port to listen on (0 = random)")
    parser.add_argument("--name", default="FoxCast-Test", help="mDNS service name")
    parser.add_argument("--interface", default="", help="Network interface IP to advertise on")
    args = parser.parse_args()

    asyncio.run(run(args.port, args.name, args.interface))


if __name__ == "__main__":
    main()
