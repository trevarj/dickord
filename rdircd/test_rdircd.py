import asyncio
import importlib.machinery
import importlib.util
import pathlib
import unittest
from unittest import mock


path = pathlib.Path(__file__).with_name("rdircd")
loader = importlib.machinery.SourceFileLoader("dickord_rdircd", str(path))
spec = importlib.util.spec_from_loader(loader.name, loader)
rdircd = importlib.util.module_from_spec(spec)
loader.exec_module(rdircd)


class Log:
    def __getattr__(self, _name):
        return lambda *_args, **_kwargs: None


class Response:
    def __init__(self, status=200, headers=None, body=None):
        self.status = status
        self.headers = headers or {}
        self.body = {} if body is None else body
        self.reason = "test"
        self.content_type = "application/json"
        self.released = False
        self.read_called = False

    async def json(self):
        return self.body

    async def text(self):
        return str(self.body)

    async def read(self):
        self.read_called = True
        return b""

    def release(self):
        self.released = True


class HistoryTests(unittest.IsolatedAsyncioTestCase):
    def make_discord(self, pages):
        discord = rdircd.Discord.__new__(rdircd.Discord)
        discord.st_eris = rdircd.adict(enabled=True)
        discord.conf = rdircd.adict(discord_msg_history_fetch_limit=20)
        discord.flake_build = lambda ts: str(int(ts))
        discord.flake_parse = lambda value: float(value)
        discord.cmd_user_cache = lambda *_args, **_kwargs: None
        discord.user_name = lambda author: author.get("username") or "unknown"
        discord.session = rdircd.adict(
            st_da=rdircd.adict(user=rdircd.adict(id="7")),
            op_msg_parse=lambda message, _guild: (
                ("" if message.get("ignore") else message.get("content", "")), rdircd.adict()
            ),
        )
        calls = []

        async def conn_req(_url, **_kwargs):
            calls.append(_kwargs["params"]["after"])
            return pages.pop(0)

        discord.conn_req = conn_req
        return discord, calls

    async def test_history_keeps_identity_and_pages_past_ignored_records(self):
        pages = [
            [
                {"id": "101", "content": "one", "author": {"id": "7", "username": "me"}},
                {"id": "102", "ignore": True, "author": {"id": "8", "username": "other"}},
            ],
            [{"id": "103", "content": "three", "author": {"id": "8", "username": "other"}}],
        ]
        discord, calls = self.make_discord(pages)
        channel = rdircd.adict(id="9", gg=rdircd.adict(id="10"))
        history = await discord.cmd_history(channel, 100, hwm=2)
        self.assertEqual(calls, ["100", "102"])
        self.assertEqual([m.msg_id for m in history.messages], ["101", "103"])
        self.assertEqual(history.cursor_ts, 103)
        self.assertTrue(history.messages[0].discord_self)
        self.assertEqual(history.messages[1].discord_user_id, "8")

    async def test_ignored_page_still_advances_cursor(self):
        discord, _calls = self.make_discord([
            [{"id": "101", "ignore": True, "author": {"id": "8", "username": "other"}}]
        ])
        history = await discord.cmd_history(
            rdircd.adict(id="9", gg=rdircd.adict(id="10")), 100, hwm=2
        )
        self.assertEqual(history.messages, [])
        self.assertEqual(history.cursor_ts, 101)


class PayloadTests(unittest.TestCase):
    def test_message_payload_restricts_mentions_and_enforces_nonce(self):
        payload = rdircd.Discord.message_payload("hello", "123", reply_msg_id="456")
        self.assertTrue(payload["enforce_nonce"])
        self.assertEqual(payload["allowed_mentions"], {"parse": ["users"], "replied_user": False})
        self.assertEqual(payload["message_reference"]["message_id"], "456")
        self.assertNotIn("roles", payload["allowed_mentions"]["parse"])
        self.assertNotIn("everyone", payload["allowed_mentions"]["parse"])

    def test_custom_and_unicode_reaction_paths(self):
        channel = rdircd.adict(
            gg=rdircd.adict(emojis={"party": rdircd.adict(name="Party", id="123")})
        )
        self.assertEqual(rdircd.Discord.reaction_api_emoji(channel, ":party:"), "Party%3A123")
        self.assertEqual(rdircd.Discord.reaction_api_emoji(channel, "👍"), "%F0%9F%91%8D")
        with self.assertRaises(rdircd.IRCBridgeSignal):
            rdircd.Discord.reaction_api_emoji(channel, ":missing:")

    def test_reaction_events_preserve_custom_identity_and_burst(self):
        session = rdircd.DiscordSession.__new__(rdircd.DiscordSession)
        session.conf = rdircd.adict(
            irc_disable_reacts_msgs=False,
            _discord_msg_old_prefix={},
            _discord_msg_old_ignore={},
        )
        session.log = Log()
        channel = rdircd.adict(
            id="20",
            users_static={},
        )
        guild = rdircd.adict(
            id="10",
            chans={"20": channel},
            emojis={"party": rdircd.adict(name="Party", id="123")},
        )
        channel.gg = guild
        received = []
        session.st_da = rdircd.adict(
            user=rdircd.adict(id="7"),
            guilds={"10": guild},
        )
        session.discord = rdircd.adict(
            flake_parse=lambda _value: 1.0,
            cmd_user_cache=lambda *_args, **_kwargs: None,
            user_name=lambda _user: "owner",
            cmd_msg_recv=lambda *_args, **kwargs: received.append(kwargs),
        )
        session.op_msg_ref_get = lambda *_args: None

        session.op_react(
            rdircd.adict(
                guild_id="10",
                channel_id="20",
                message_id="30",
                user_id="7",
                member=rdircd.adict(user=rdircd.adict(id="7")),
                emoji=rdircd.adict(id="123", name="Party"),
                burst=True,
            ),
            "add",
        )
        self.assertEqual(received[-1]["discord_event"], "react-add")
        self.assertEqual(received[-1]["discord_emoji"], ":Party:")
        self.assertEqual(received[-1]["discord_emoji_api"], "Party:123")
        self.assertTrue(received[-1]["discord_burst"])

        session.op_react(
            rdircd.adict(
                guild_id="10",
                channel_id="20",
                message_id="30",
                emoji=rdircd.adict(id="123", name=None),
            ),
            "remove_emoji",
        )
        self.assertEqual(received[-1]["discord_event"], "react-remove-emoji")
        self.assertEqual(received[-1]["discord_emoji_api"], "Party:123")

    def test_structured_tags_escape_and_validate(self):
        protocol = rdircd.IRCProtocol.__new__(rdircd.IRCProtocol)
        protocol.log = Log()
        tags = protocol.dickord_tags(
            discord_msg_id="123",
            discord_user_id="456",
            discord_event="react-add",
            discord_emoji="x; y\\z",
            discord_emoji_api="x:123",
            discord_self=True,
            discord_burst=True,
        )
        self.assertIn("+dickord/discord-msgid=123", tags)
        self.assertIn("+dickord/discord-userid=456", tags)
        self.assertIn("+dickord/event=react-add", tags)
        self.assertIn("+dickord/emoji=x\\:\\sy\\\\z", tags)
        self.assertIn("+dickord/discord-emoji=x:123", tags)
        self.assertIn("+dickord/self=1", tags)
        self.assertNotIn("not-a-snowflake", protocol.dickord_tags(discord_msg_id="not-a-snowflake"))


class HTTPTests(unittest.IsolatedAsyncioTestCase):
    def make_session(self):
        session = rdircd.DiscordSession.__new__(rdircd.DiscordSession)
        session.conf = rdircd.adict(
            discord_http_delay_padding=0,
            discord_http_timeout_conn=40,
            discord_http_timeout_conn_sock=30,
            discord_api_user_agent="test",
            auth_token_manual=True,
        )
        session.api_url = "https://discord.com/api/v10/"
        session.rate_limits = rdircd.adict(states={}, routes={}, locks={}, global_until=0)
        session.log = session.log_http = Log()
        session._repr = repr
        session.ws_enabled = True
        session.st_da = rdircd.adict(state="ready")
        session.state = lambda state: session.st_da.update(state=state)
        return session

    def test_route_normalization_and_timeout(self):
        route, major = rdircd.DiscordSession.rate_limit_route(
            "delete", "https://discord.com/api/v10/channels/123/messages/456"
        )
        self.assertEqual(route, "DELETE /api/v10/channels/123/messages/:id")
        self.assertEqual(major, "channels:123")
        session = self.make_session()
        timeout = session.http_timeout()
        self.assertEqual(timeout.total, 40)
        self.assertEqual(timeout.sock_read, 40)
        self.assertEqual(timeout.sock_connect, 30)

    async def test_raw_response_is_consumed_and_released(self):
        session = self.make_session()
        response = Response(status=204)

        class HTTP:
            async def request(self, *_args, **_kwargs):
                return response

        session.ws = rdircd.adict(http=HTTP())
        result = await session.req("channels/123/typing", m="post", auth=False, raw=True)
        self.assertIsNone(result)
        self.assertTrue(response.read_called)
        self.assertTrue(response.released)

    async def test_manual_token_401_stops_without_retry(self):
        session = self.make_session()
        response = Response(status=401)
        calls = 0

        class HTTP:
            async def request(self, *_args, **_kwargs):
                nonlocal calls
                calls += 1
                return response

        session.ws = rdircd.adict(http=HTTP())
        session.req_auth_token = mock.AsyncMock(return_value="token")
        with self.assertRaises(rdircd.DiscordSessionError):
            await session.req("users/@me")
        self.assertEqual(calls, 1)
        self.assertFalse(session.ws_enabled)

    async def test_managed_token_refreshes_once(self):
        session = self.make_session()
        session.conf.auth_token_manual = False
        responses = [Response(status=401), Response(body={"id": "7"})]

        class HTTP:
            async def request(self, *_args, **_kwargs):
                return responses.pop(0)

        session.ws = rdircd.adict(http=HTTP())
        session.auth_token = "old"
        session.req_auth_token = mock.AsyncMock(side_effect=["old", "new"])
        result = await session.req("users/@me")
        self.assertEqual(result, {"id": "7"})
        self.assertEqual(session.req_auth_token.await_count, 2)
        self.assertTrue(session.ws_enabled)

    async def test_bucket_serializes_requests_and_global_429_retries(self):
        session = self.make_session()
        route = ("GET /channels/123/messages", "channels:123")
        session.rate_limits.routes[route[0]] = "bucket:channels:123"
        active = maximum = 0

        async def request():
            nonlocal active, maximum
            active += 1
            maximum = max(maximum, active)
            await asyncio.sleep(0.005)
            active -= 1
            return Response(headers={"X-RateLimit-Bucket": "bucket"})

        await asyncio.gather(
            session.rate_limit_wrapper(route, request),
            session.rate_limit_wrapper(route, request),
        )
        self.assertEqual(maximum, 1)

        responses = [
            Response(status=429, body={"retry_after": 0.001, "global": True}),
            Response(headers={"X-RateLimit-Bucket": "bucket"}),
        ]

        async def limited():
            return responses.pop(0)

        result = await session.rate_limit_wrapper(route, limited)
        self.assertEqual(result.status, 200)
        self.assertGreater(session.rate_limits.global_until, 0)


class GatewayTests(unittest.IsolatedAsyncioTestCase):
    def make_session(self):
        session = rdircd.DiscordSession.__new__(rdircd.DiscordSession)
        session.log = Log()
        session.st_da = rdircd.adict(session_id="sid", seq=4)
        session.ws_state = rdircd.adict(closed=asyncio.Event(), usable=asyncio.Event())
        session.ws_state.usable.set()
        session.guild_sub_bulk = True
        session.guild_sub_probe_ts = 0
        session.ws_enabled = True
        session.state = lambda state: session.st_da.update(state=state)
        return session

    def test_close_codes_choose_resume_reidentify_backoff_or_stop(self):
        session = self.make_session()
        self.assertEqual(session.ws_close_classify(4000), "resume")
        self.assertEqual(session.ws_close_classify(4007), "reidentify")
        self.assertIsNone(session.st_da.session_id)
        session.st_da.session_id = "sid"
        self.assertEqual(session.ws_close_classify(4008), "backoff")
        self.assertEqual(session.ws_close_classify(4004), "stop")
        self.assertFalse(session.ws_enabled)

    def test_opcode_37_payload_and_explicit_14_fallback(self):
        session = self.make_session()
        channel = rdircd.adict(id="20", ct=rdircd.DiscordSession.c_chan_type.text)
        guild = rdircd.adict(id="10", synced=False, chans={"20": channel})
        session.st_da.guilds = {"10": guild}
        session.conf = rdircd.adict(discord_only_track_irc_joined=False)
        sent = []
        session.ws_send = lambda op, data: sent.append((op, data))
        session.ws_req_guild_sync()
        self.assertEqual(sent[0][0], rdircd.DiscordSession.c.ox_guild_sub_bulk)
        self.assertIn("10", sent[0][1]["subscriptions"])

        session.guild_sub_probe_ts = rdircd.time.monotonic()
        self.assertEqual(session.ws_close_classify(4001), "reidentify")
        self.assertFalse(session.guild_sub_bulk)
        guild.synced = False
        sent.clear()
        session.ws_req_guild_sync()
        self.assertEqual(sent[0][0], rdircd.DiscordSession.c.ox_guild_sub)

    async def test_heartbeat_reconnects_after_one_missed_ack(self):
        session = self.make_session()
        session.st_da.hb_pending = False
        sent = []
        session.ws_send = lambda op, data: sent.append((op, data))

        def close():
            session.ws_state.closed.set()

        session.ws_close_later = close
        with mock.patch.object(rdircd.random, "random", return_value=0):
            await session.op_heartbeat_task(0.001)
        self.assertEqual(len(sent), 1)
        self.assertTrue(session.ws_state.closed.is_set())

    async def test_hello_starts_one_heartbeat_task(self):
        session = self.make_session()
        tasks = []

        class Tasks:
            def add(self, coroutine):
                task = asyncio.create_task(coroutine)
                tasks.append(task)
                return task

        session.ws = rdircd.adict(tasks=Tasks(), handlers={})
        session.ws_add_handler = lambda *_args, **_kwargs: None
        session.op_hello_auth = mock.AsyncMock()
        with mock.patch.object(rdircd.random, "random", return_value=1):
            result = await session.op_hello(rdircd.adict(d=rdircd.adict(heartbeat_interval=1000)))
        self.assertEqual(result, rdircd.DiscordSession.c.oneshot)
        self.assertEqual(len(tasks), 1)
        self.assertIs(session.st_da.hb_task, tasks[0])
        tasks[0].cancel()
        with self.assertRaises(asyncio.CancelledError):
            await tasks[0]


if __name__ == "__main__":
    unittest.main()
