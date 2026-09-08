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
            op_msg_reply_id=rdircd.DiscordSession.op_msg_reply_id,
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
            [{
                "id": "103",
                "content": "three",
                "type": 19,
                "author": {"id": "8", "username": "other"},
                "message_reference": {"message_id": "101"},
                "referenced_message": {"id": "101"},
            }],
        ]
        discord, calls = self.make_discord(pages)
        channel = rdircd.adict(id="9", gg=rdircd.adict(id="10"))
        history = await discord.cmd_history(channel, 100, hwm=2)
        self.assertEqual(calls, ["100", "102"])
        self.assertEqual([m.msg_id for m in history.messages], ["101", "103"])
        self.assertEqual(history.cursor_ts, 103)
        self.assertTrue(history.messages[0].discord_self)
        self.assertEqual(history.messages[1].discord_user_id, "8")
        self.assertEqual(history.messages[1].discord_reply_msg_id, "101")

    async def test_ignored_page_still_advances_cursor(self):
        discord, _calls = self.make_discord([
            [{"id": "101", "ignore": True, "author": {"id": "8", "username": "other"}}]
        ])
        history = await discord.cmd_history(
            rdircd.adict(id="9", gg=rdircd.adict(id="10")), 100, hwm=2
        )
        self.assertEqual(history.messages, [])
        self.assertEqual(history.cursor_ts, 101)


class AttachmentTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.conf = rdircd.RDIRCDConfigBase()
        self.conf.irc_names_join = self.conf.discord_embed_info = False
        self.conf.discord_msg_interact_cache = False
        self.conf._irc_dedup_interval = 0
        self.conf._discord_msg_old_prefix = self.conf._discord_msg_old_ignore = {}
        self.conf.recv_filters = self.conf.recv_repls = self.conf.unmon_filters = {}
        for key in (
            "irc_len_dont_split_re", "discord_terminal_links_re",
            "discord_embed_info_len_skip_re",
        ):
            setattr(self.conf, "_" + key, rdircd.re.compile(getattr(self.conf, key)))
        self.bridge = rdircd.RDIRCD.__new__(rdircd.RDIRCD)
        self.bridge.conf, self.bridge.log = self.conf, Log()
        self.bridge.irc_conns = []
        self.bridge.st_br = rdircd.adict(
            did_chan={"20": "test"}, line_dedup=None, d2i={}, i2d={}
        )
        self.channel = rdircd.adict(
            id="20", did="20", name="test", tid=None, private=False,
            users=rdircd.TimedCacheDict(60),
            gg=rdircd.adict(id="10", chans={}),
        )
        self.channel.gg.chans["20"] = self.channel
        self.protocol = rdircd.IRCProtocol.__new__(rdircd.IRCProtocol)
        self.protocol.conf, self.protocol.log = self.conf, Log()
        self.protocol.st_irc = rdircd.adict(
            nick="bridge", chans={"test"}, typing_repeat=rdircd.adict(active={})
        )
        self.wire = []
        self.protocol.data_send = self.wire.append
        self.bridge.cmd_chan_conns = lambda _name: [self.protocol]
        self.bridge.cmd_msg_monitor = lambda *_args, **_kwargs: None
        self.discord = rdircd.Discord.__new__(rdircd.Discord)
        self.discord.bridge, self.discord.conf, self.discord.log = self.bridge, self.conf, Log()
        self.discord.st_eris = rdircd.adict(enabled=True)
        self.discord.flake_parse = lambda value: float(value) if value else None
        self.discord.flake_build = lambda value: str(int(value))
        self.discord.cmd_user_cache = lambda *_args, **_kwargs: None
        self.discord.user_name = lambda user: user.get("author", user)["username"]
        self.session = rdircd.DiscordSession.__new__(rdircd.DiscordSession)
        self.session.conf, self.session.log, self.session._repr = self.conf, Log(), repr
        self.session.discord, self.discord.session = self.discord, self.session
        self.session.st_da = rdircd.adict(
            user={"id": "7"}, guilds={"10": self.channel.gg}, embed_info={}, icache={}
        )

    def message(self, content="", urls=(), **kwargs):
        message = rdircd.adict(
            id="101", type=19, guild_id="10", channel_id="20",
            author={"id": "7", "username": "owner"}, content=content,
            attachments=[{"url": url} for url in urls],
            message_reference={"message_id": "100"},
        )
        message.update(kwargs)
        return message

    def frames(self):
        return [self.protocol._parse(line.rstrip(b"\r\n")) for line in self.wire]

    def test_parser_tags_only_actual_attachments_with_terminal_links(self):
        urls = [
            "https://cdn.discordapp.com/attachments/10/20/photo.png?ex=1&hm=a;b",
            "https://cdn.discordapp.com/attachments/10/21/video.mp4?ex=2",
        ]
        self.conf.irc_prefix_attachment = "file: "
        for terminal_links in (False, True):
            with self.subTest(terminal_links=terminal_links):
                self.wire.clear()
                self.conf.discord_terminal_links = terminal_links
                self.session.op_msg(self.message("file: " + urls[0], urls), "create")
                frames = self.frames()
                self.assertEqual(
                    [frame.tags.get("+dickord/attachment") for frame in frames],
                    [None, *urls],
                )
                self.assertEqual(
                    [frame.tags.get("+dickord/discord-reply-msgid") for frame in frames],
                    ["100", None, None],
                )
                for frame in frames:
                    self.assertEqual(frame.tags["+dickord/discord-msgid"], "101")
                    self.assertEqual(frame.tags["+dickord/discord-userid"], "7")
                    self.assertEqual(frame.tags["+dickord/self"], "1")
                    self.assertEqual(frame.src, ":owner!owner@discord")
                self.assertIn(b"hm=a\\:b", self.wire[1])
                self.assertEqual("\x1b]8;;" in frames[1].params[1], terminal_links)

        self.wire.clear()
        self.session.op_msg(self.message("file: " + urls[0]), "create")
        self.assertEqual([frame.tags.get("+dickord/attachment") for frame in self.frames()], [None])

    async def test_history_attachment_tags_survive_replay_to_another_client(self):
        url = "https://cdn.discordapp.com/attachments/10/20/photo.png?ex=1"
        self.conf.irc_prefix_pinned = "saved: "
        self.discord.conn_req = mock.AsyncMock(return_value=[
            self.message(urls=[url], pinned=True)
        ])
        history = await self.discord.cmd_history(self.channel, 100, hwm=2)
        message = history.messages[0]
        for _ in range(2):
            self.bridge.cmd_msg_discord(
                self.channel, message.nick, "[history] " + message.line,
                tags=message.tags, conn=self.protocol,
                discord_msg_id=message.msg_id, discord_user_id=message.discord_user_id,
                discord_reply_msg_id=message.discord_reply_msg_id,
                discord_self=message.discord_self,
            )
        self.assertEqual(self.wire[0], self.wire[1])
        self.assertEqual(len(self.wire), 2)
        frame = self.frames()[1]
        self.assertEqual(frame.tags["+dickord/attachment"], url)
        self.assertEqual(frame.tags["+dickord/discord-reply-msgid"], "100")
        self.assertEqual(frame.tags["+dickord/self"], "1")
        self.assertEqual(frame.params[1], "saved: [history] " + self.conf.irc_prefix_attachment + url)

    def test_live_normal_and_snapshot_attachments_keep_their_raw_urls(self):
        urls = [
            "https://cdn.discordapp.com/attachments/10/20/photo.png?ex=1",
            "https://cdn.discordapp.com/attachments/10/21/video.mp4?hm=" + "a" * 600,
            "https://cdn.discordapp.com/attachments/10/22/other.png?ex=3",
        ]
        self.conf.discord_embed_info = True
        self.session.op_msg(self.message(
            "caption", urls[:1],
            message_snapshots=[
                {"message": self.message("forwarded", [url])} for url in urls[1:]
            ],
        ), "create")
        attachments = [
            frame for frame in self.frames() if "+dickord/attachment" in frame.tags
        ]
        self.assertEqual([frame.tags["+dickord/attachment"] for frame in attachments], urls)
        self.assertNotIn(urls[1], attachments[1].params[1])  # Existing snapshot preview is truncated.
        self.assertTrue(all(frame.tags["+dickord/self"] == "1" for frame in attachments))

    def test_long_signed_attachment_is_one_attributed_edit_frame(self):
        url = "https://cdn.discordapp.com/attachments/10/20/photo.png?hm="
        url += "a" * (2048 - len(url))
        self.session.op_msg(self.message(urls=[url]), "update")
        self.assertEqual(len(self.wire), 1)
        self.assertLessEqual(len(self.wire[0]), 8192)
        frame = self.frames()[0]
        self.assertEqual(frame.tags["+dickord/attachment"], url)
        self.assertEqual(frame.tags["+dickord/discord-msgid"], "101")
        self.assertEqual(frame.tags["+dickord/discord-userid"], "7")
        self.assertEqual(frame.tags["+dickord/discord-reply-msgid"], "100")
        self.assertEqual(frame.tags["+dickord/self"], "1")
        self.assertEqual(frame.tags["+dickord/event"], "edit")
        self.assertEqual(frame.params[1], self.conf.irc_prefix_edit + self.conf.irc_prefix_attachment + url)

    def test_attachment_limits_fall_back_without_duplicate_upload_frames(self):
        url = "https://cdn.discordapp.com/attachments/10/20/photo.png?hm="
        for attachment, prefix in (
            (url + "a" * (2049 - len(url)), "file: "),
            (url + "a", "padding " * 1100),
        ):
            with self.subTest(url_length=len(attachment), prefix_length=len(prefix)):
                self.wire.clear()
                self.conf.irc_prefix_attachment = prefix
                self.session.op_msg(self.message(urls=[attachment]), "create")
                frames = self.frames()
                self.assertEqual(" ".join(f.params[1] for f in frames).split(), (prefix + attachment).split())
                self.assertTrue(all("+dickord/attachment" not in f.tags for f in frames))
                self.assertTrue(all(f.tags["+dickord/discord-msgid"] == "101" for f in frames))
                self.assertTrue(all(f.tags["+dickord/self"] == "1" for f in frames))
                self.assertEqual(
                    [f.tags["+dickord/discord-reply-msgid"] for f in frames
                     if "+dickord/discord-reply-msgid" in f.tags],
                    ["100"],
                )

    def test_long_text_splits_preserve_escaped_tags_and_only_first_reply(self):
        text, emoji = "caption " * 100 + "\n" + "continued " * 80, "x; y\\n"
        self.protocol.cmd_msg_chan(
            "owner", "test", text, notice=True, discord_msg_id="101",
            discord_user_id="7", discord_reply_msg_id="100",
            discord_self=True, discord_emoji=emoji,
        )
        frames = self.frames()
        self.assertEqual(" ".join(frame.params[1] for frame in frames).split(), text.split())
        for frame in frames:
            self.assertEqual(frame.cmd, "notice")
            self.assertEqual(frame.tags["+dickord/discord-msgid"], "101")
            self.assertEqual(frame.tags["+dickord/discord-userid"], "7")
            self.assertEqual(frame.tags["+dickord/self"], "1")
            self.assertEqual(frame.tags["+dickord/emoji"], emoji)
        self.assertEqual(frames[0].tags["+dickord/discord-reply-msgid"], "100")
        self.assertTrue(all("+dickord/discord-reply-msgid" not in f.tags for f in frames[1:]))


    def test_receive_replacements_keep_provenance_when_rendered_lines_collide(self):
        url = "https://cdn.discordapp.com/attachments/10/20/photo.png?ex=1"
        self.conf.irc_prefix_attachment = "file: "
        self.conf.recv_repls = {"*": [
            rdircd.adict(re=rdircd.re.compile(r"^drop$"), sub=None, tsb=Log()),
            rdircd.adict(re=rdircd.re.compile(r"^file: .*"), sub="file preview", tsb=Log()),
        ]}
        self.bridge.uid = lambda *_args, **_kwargs: "guild:test"
        self.session.op_msg(self.message("drop\nfile: " + url, [url]), "create")
        frames = self.frames()
        self.assertEqual([frame.params[1] for frame in frames], ["file preview", "file preview"])
        self.assertEqual([frame.tags.get("+dickord/attachment") for frame in frames], [None, url])


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

    def test_reply_id_only_accepts_native_message_references(self):
        reply = rdircd.adict(
            type=19,
            message_reference=rdircd.adict(message_id="123"),
            referenced_message=rdircd.adict(id="123"),
        )
        self.assertEqual(rdircd.DiscordSession.op_msg_reply_id(reply), "123")
        reply.message_reference.type = 1
        self.assertIsNone(rdircd.DiscordSession.op_msg_reply_id(reply))
        reply.message_reference.type = 0
        reply.referenced_message = None
        self.assertEqual(rdircd.DiscordSession.op_msg_reply_id(reply), "123")
        reply.type = 0
        self.assertIsNone(rdircd.DiscordSession.op_msg_reply_id(reply))

    def test_structured_tags_escape_and_validate(self):
        protocol = rdircd.IRCProtocol.__new__(rdircd.IRCProtocol)
        protocol.log = Log()
        tags = protocol.dickord_tags(
            discord_msg_id="123",
            discord_user_id="456",
            discord_reply_msg_id="789",
            discord_event="react-add",
            discord_emoji="x; y\\z",
            discord_emoji_api="x:123",
            discord_self=True,
            discord_burst=True,
        )
        self.assertIn("+dickord/discord-msgid=123", tags)
        self.assertIn("+dickord/discord-userid=456", tags)
        self.assertIn("+dickord/discord-reply-msgid=789", tags)
        self.assertIn("+dickord/event=react-add", tags)
        self.assertIn("+dickord/emoji=x\\:\\sy\\\\z", tags)
        self.assertIn("+dickord/discord-emoji=x:123", tags)
        self.assertIn("+dickord/self=1", tags)
        self.assertNotIn("not-a-snowflake", protocol.dickord_tags(discord_msg_id="not-a-snowflake"))


class MessageSendTests(unittest.IsolatedAsyncioTestCase):
    async def test_rest_response_confirms_without_gateway_echo(self):
        discord = rdircd.Discord.__new__(rdircd.Discord)
        discord.st_eris = rdircd.adict(enabled=True, msg_confirms={})
        discord.conf = rdircd.adict(
            discord_thread_redirect_prefixed_responses_from_parent_chan=False,
            discord_thread_id_prefix="=",
            discord_msg_confirm_timeout=1,
            state_watch=lambda *_args: None,
        )
        discord.bridge = rdircd.adict(uid_start="test")
        discord.log = Log()
        discord._repr = repr
        discord.flake_build = lambda _ts: "123"
        discord.cmd_msg_parse_flags = lambda line: (line, 0)
        discord.cmd_msg_emojify = lambda _guild, line: line
        discord.cmd_msg_mentionify = mock.AsyncMock(side_effect=lambda _guild, line: line)
        discord.conn_req = mock.AsyncMock(return_value={"id": "999"})
        channel = rdircd.adict(
            id="20",
            name="test",
            gg=rdircd.adict(id="10"),
            threads={},
            last_msg_sent=rdircd.adict(),
        )

        result = await discord.cmd_msg_send(channel, "hello")
        self.assertEqual(result, "999")
        self.assertIn("123", discord.st_eris.msg_confirms)
        future = discord.st_eris.msg_confirms["123"]
        discord.msg_confirm_expire("123", future)
        self.assertNotIn("123", discord.st_eris.msg_confirms)

    @staticmethod
    def voice_response(body, *, status=200, content_type="audio/ogg", length=None):
        class Content:
            async def iter_chunked(self, _size):
                yield body

        response = rdircd.adict(
            status=status,
            headers={"Content-Length": str(len(body) if length is None else length)},
            content_type=content_type,
            content=Content(),
            released=False,
        )
        response.release = lambda: response.update(released=True)
        return response

    def make_voice_discord(self, response):
        discord = rdircd.Discord.__new__(rdircd.Discord)
        discord.st_eris = rdircd.adict(enabled=True, msg_confirms={})
        discord.conf = rdircd.adict(
            discord_thread_redirect_prefixed_responses_from_parent_chan=False,
            discord_thread_id_prefix="=",
            discord_msg_confirm_timeout=1,
            state_watch=mock.Mock(),
        )
        discord.bridge = rdircd.adict(uid_start="test")
        discord.log = Log()
        discord._repr = repr
        discord.flake_build = lambda _ts: "123"
        request = mock.AsyncMock(return_value=response)
        discord.session = rdircd.adict(ws=rdircd.adict(http=rdircd.adict(request=request)))
        discord.conn_req = mock.AsyncMock(return_value={"id": "999"})
        channel = rdircd.adict(
            id="20",
            name="test",
            gg=rdircd.adict(id="10"),
            threads={},
            last_msg_sent=rdircd.adict(flake="old", line="old"),
        )
        return discord, channel, request

    def test_privmsg_parses_both_voice_tags(self):
        protocol = rdircd.IRCProtocol.__new__(rdircd.IRCProtocol)
        protocol.cmd_msg_from_irc = mock.Mock()
        protocol.recv_cmd_privmsg(rdircd.adict(
            params=["#test", "https://files.example/voice.ogg"],
            tags={
                "+dickord/voice-duration": "1.25",
                "+dickord/voice-waveform": "AQI=",
            },
        ))
        protocol.cmd_msg_from_irc.assert_called_once_with(
            "#test", "https://files.example/voice.ogg", from_self=True,
            ergo_msg_id=None, discord_reply_msg_id=None,
            voice_duration="1.25", voice_waveform="AQI=",
        )

    async def test_voice_send_downloads_without_auth_and_posts_exact_multipart(self):
        audio = b"OggS" + b"\0" * 22 + b"\1" + b"\x13" + b"OpusHead\x01\x01" + b"\0" * 9
        response = self.voice_response(audio)
        discord, channel, request = self.make_voice_discord(response)

        class FormData:
            def __init__(self, **kwargs):
                self.fields = []
                self.kwargs = kwargs

            def add_field(self, name, value, **kwargs):
                self.fields.append((name, value, kwargs))

        url = "https://files.example/voice.ogg"
        with mock.patch.object(rdircd.aiohttp, "FormData", FormData):
            result = await discord.cmd_msg_send_voice(
                channel, url, "1.25", "AQI=", reply_msg_id="456"
            )

        self.assertEqual(result, "999")
        request.assert_awaited_once_with(
            "get", url, allow_redirects=False, auto_decompress=False
        )
        discord.conn_req.assert_awaited_once()
        post = discord.conn_req.await_args
        self.assertEqual(post.args, ("channels/20/messages",))
        self.assertEqual(post.kwargs["m"], "post")
        self.assertNotIn("json", post.kwargs)
        fields = post.kwargs["data"].fields
        self.assertEqual([field[0] for field in fields], ["payload_json", "files[0]"])
        self.assertEqual(post.kwargs["data"].kwargs, {"quote_fields": False})
        payload = rdircd.json.loads(fields[0][1])
        self.assertEqual(payload, {
            "nonce": "123",
            "enforce_nonce": True,
            "flags": 8192,
            "allowed_mentions": {"parse": ["users"], "replied_user": False},
            "message_reference": {
                "message_id": "456",
                "fail_if_not_exists": False,
            },
            "attachments": [{
                "id": 0,
                "filename": "voice-message.ogg",
                "duration_secs": 1.25,
                "waveform": "AQI=",
            }],
        })
        self.assertEqual(fields[0][2], {"content_type": "application/json"})
        self.assertEqual(fields[1], (
            "files[0]",
            audio,
            {"filename": "voice-message.ogg", "content_type": "audio/ogg"},
        ))
        self.assertEqual(channel.last_msg_sent, {"flake": "old", "line": "old"})
        self.assertTrue(response.released)
        self.assertIn("123", discord.st_eris.msg_confirms)
        discord.msg_confirm_expire("123", discord.st_eris.msg_confirms["123"])

    async def test_invalid_voice_metadata_and_download_never_post(self):
        audio = b"OggS" + b"\0" * 22 + b"\1" + b"\x13" + b"OpusHead\x01\x01" + b"\0" * 9
        for duration, waveform in [("0", "AQI="), ("1", "not-base64")]:
            with self.subTest(duration=duration, waveform=waveform):
                discord, channel, request = self.make_voice_discord(
                    self.voice_response(audio)
                )
                with self.assertRaises(rdircd.IRCBridgeSignal):
                    await discord.cmd_msg_send_voice(
                        channel, "https://files.example/voice.ogg", duration, waveform
                    )
                request.assert_not_awaited()
                discord.conn_req.assert_not_awaited()

        for response in [
            self.voice_response(audio, status=302),
            self.voice_response(audio, content_type="text/plain"),
            self.voice_response(b"not an ogg opus file"),
            self.voice_response(b"OggS" + b"\0" * 22 + b"\1" + b"\x08" + b"OpusHead"),
        ]:
            with self.subTest(status=response.status, content_type=response.content_type):
                discord, channel, _request = self.make_voice_discord(response)
                with self.assertRaises(rdircd.IRCBridgeSignal):
                    await discord.cmd_msg_send_voice(
                        channel, "https://files.example/voice.ogg", "1", "AQI="
                    )
                discord.conn_req.assert_not_awaited()

    async def test_queue_routes_voice_tags_without_changing_plain_url_path(self):
        bridge = rdircd.RDIRCD.__new__(rdircd.RDIRCD)
        bridge.irc_msg_queue = asyncio.Queue()
        bridge.irc_chans_sys = {}
        bridge.irc_msg_translate_preq = lambda line: line
        bridge.irc_msg_translate_postq = mock.Mock(side_effect=lambda _info, line: line)
        bridge.irc_discord_info = lambda _name: rdircd.adict(cc="channel")
        bridge._repr = repr
        bridge.conf = rdircd.adict(
            _discord_msg_edit_re=rdircd.re.compile(r"(?!x)x"),
            _discord_msg_del_re=rdircd.re.compile(r"(?!x)x"),
        )
        bridge.discord = rdircd.adict(
            cmd_msg_send_voice=mock.AsyncMock(return_value="100"),
            cmd_msg_send=mock.AsyncMock(return_value="101"),
        )
        conn = rdircd.adict(
            chan_name=lambda _chan: "test",
            send=mock.Mock(),
            cmd_msg_chan_sys=mock.Mock(),
        )
        url = "https://files.example/voice.ogg"
        bridge.irc_msg(
            conn, "#test", url, ergo_msg_id="voice-ergo",
            voice_duration="1", voice_waveform="AQI=",
        )
        bridge.irc_msg(conn, "#test", url, ergo_msg_id="text-ergo")
        bridge.irc_msg_queue.put_nowait(StopIteration)

        await bridge.irc_msg_queue_proc()

        bridge.discord.cmd_msg_send_voice.assert_awaited_once_with(
            "channel", url, "1", "AQI=", reply_msg_id=None
        )
        bridge.discord.cmd_msg_send.assert_awaited_once_with(
            "channel", url, reply_msg_id=None
        )
        self.assertEqual(conn.send.call_count, 2)


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
