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


class AvatarURLTests(unittest.TestCase):
    def test_user_avatar_urls_cover_custom_modern_legacy_and_partial_payloads(self):
        self.assertEqual(
            rdircd.discord_user_avatar_url({
                "id": "8", "avatar": "a_ab12", "discriminator": "0",
            }),
            "https://cdn.discordapp.com/avatars/8/a_ab12.png?size=256",
        )
        modern_id = str(5 << 22)
        self.assertEqual(
            rdircd.discord_user_avatar_url({
                "id": modern_id, "avatar": None, "discriminator": "0",
            }),
            "https://cdn.discordapp.com/embed/avatars/5.png",
        )
        self.assertEqual(
            rdircd.discord_user_avatar_url({
                "id": "8", "avatar": None, "discriminator": "1234",
            }),
            "https://cdn.discordapp.com/embed/avatars/4.png",
        )
        for user in (
            {"id": "8"},
            {"id": "8", "avatar": None},
            {"avatar": "abcd"},
            {"id": "not-numeric", "avatar": "abcd"},
            {"id": "8", "avatar": ""},
            {"id": "8", "avatar": None, "discriminator": "legacy"},
        ):
            with self.subTest(user=user):
                self.assertIsNone(rdircd.discord_user_avatar_url(user))

    def test_guild_icon_urls_distinguish_removal_from_missing_data(self):
        self.assertEqual(
            rdircd.discord_guild_icon_url({"id": "10", "icon": "abcd"}),
            "https://cdn.discordapp.com/icons/10/abcd.png?size=256",
        )
        self.assertEqual(rdircd.discord_guild_icon_url({"id": "10", "icon": None}), "")
        self.assertIsNone(rdircd.discord_guild_icon_url({"id": "10"}))
        self.assertIsNone(rdircd.discord_guild_icon_url({"id": "x", "icon": "abcd"}))

    def test_channel_icon_urls_fail_closed_on_missing_or_invalid_data(self):
        for channel in (
            None,
            {"id": "30"},
            {"id": "30", "icon": None},
            {"icon": "abcd"},
            {"id": "30/31", "icon": "abcd"},
            {"id": "３０", "icon": "abcd"},
            {"id": "30", "icon": 123},
            {"id": "30", "icon": ""},
            {"id": "30", "icon": "../abcd"},
        ):
            with self.subTest(channel=channel):
                self.assertIsNone(rdircd.discord_channel_icon_url(channel))


class AttachmentTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.conf = rdircd.RDIRCDConfigBase()
        self.conf.irc_names_join = self.conf.discord_embed_info = False
        self.conf.discord_msg_interact_cache = False
        self.conf._irc_dedup_interval = 0
        self.conf._irc_names_timeout = 60
        self.conf._discord_name_preference_order = ["nick", "display", "login"]
        self.conf.renames = {}
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
            ct=rdircd.DiscordSession.c_chan_type.text, parent_id=None,
            names=rdircd.adict(raw="test"), threads={},
            users=rdircd.TimedCacheDict(60),
            gg=rdircd.adict(id="10", name="Guild; Hall", chans={}),
        )
        self.channel.gg.chans["20"] = self.channel
        self.protocol = rdircd.IRCProtocol.__new__(rdircd.IRCProtocol)
        self.protocol.conf, self.protocol.log = self.conf, Log()
        self.protocol.st_irc = rdircd.adict(
            nick="bridge", user="bridge", host="rdircd", ts_watch=None,
            chans={"test": rdircd.adict(topic="")},
            typing_repeat=rdircd.adict(active={}),
        )
        self.wire = []
        self.protocol.data_send = self.wire.append
        self.bridge.cmd_chan_conns = lambda _name: [self.protocol]
        self.bridge.cmd_msg_monitor = lambda *_args, **_kwargs: None
        self.discord = rdircd.Discord.__new__(rdircd.Discord)
        self.discord.bridge, self.discord.conf, self.discord.log = self.bridge, self.conf, Log()
        self.bridge.discord = self.discord
        self.discord.st_eris = rdircd.adict(enabled=True)
        self.discord.flake_parse = lambda value: float(value) if value else None
        self.discord.flake_build = lambda value: str(int(value))
        self.discord.cmd_user_cache = lambda *_args, **_kwargs: None
        self.discord.user_name = lambda user: user.get("author", user)["username"]
        self.session = rdircd.DiscordSession.__new__(rdircd.DiscordSession)
        self.session.conf, self.session.log, self.session._repr = self.conf, Log(), repr
        self.session.discord, self.discord.session = self.discord, self.session
        me = rdircd.adict(id=1, name="me", chans={})
        self.session.st_da = rdircd.adict(
            user={"id": "7", "username": "owner", "global_name": "Owner"}, me=me,
            guilds={1: me, "10": self.channel.gg}, embed_info={}, icache={}
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

    def process_channel(self, guild, **payload):
        channels, _ = self.session.op_ev_chans_process(
            guild, rdircd.adict(payload), dedup=True,
        )
        guild.chans.update(channels)
        return guild.chans[payload["id"]]

    def test_live_avatar_uses_global_author_on_every_line(self):
        author = rdircd.adict(
            id="8", username="other", avatar="a_ab12", discriminator="0",
        )
        self.session.op_msg(self.message(
            "first\nsecond", author=author, member=rdircd.adict(avatar="guild_hash"),
        ), "create")
        frames = self.frames()
        self.assertEqual([frame.params[1] for frame in frames], ["first", "second"])
        self.assertTrue(all(
            frame.tags["+dickord/avatar"]
            == "https://cdn.discordapp.com/avatars/8/a_ab12.png?size=256"
            for frame in frames
        ))
        self.wire.clear()
        self.session.op_msg(self.message("missing avatar"), "create")
        self.assertNotIn("+dickord/avatar", self.frames()[0].tags)

    def test_initial_channel_metadata_and_guild_icon(self):
        self.protocol.bridge = self.bridge
        self.conf._irc_nick_sys = self.conf.irc_nick_sys
        self.bridge.server_host = "rdircd"
        self.bridge.cmd_chan_map_sync_tracking = lambda *_args, **_kwargs: None
        self.bridge.cmd_chan_names = lambda *_args, **_kwargs: []
        self.protocol.st_irc.chans.pop("test")
        channel = rdircd.adict(name="test", topic="", cc=self.channel)
        channel_map = rdircd.adict(test=channel)
        channel_map.ø_online = False
        self.channel.gg.icon = "abcd"
        self.protocol.cmd_join("#test", cm=channel_map)
        tagmsg = [frame for frame in self.frames() if frame.cmd == "tagmsg"]
        self.assertEqual(len(tagmsg), 1)
        self.assertEqual(
            tagmsg[0].tags["+dickord/channel"],
            '{"v":1,"guild_id":"10","guild_name":"Guild; Hall",'
            '"channel_id":"20","channel_type":0,"parent_id":null,"channel_name":"test",'
            '"guild_icon_url":"https://cdn.discordapp.com/icons/10/abcd.png?size=256",'
            '"channel_icon_url":null}',
        )
        self.assertEqual(
            tagmsg[0].tags["+dickord/guild-icon"],
            "https://cdn.discordapp.com/icons/10/abcd.png?size=256",
        )
        self.assertEqual(tagmsg[0].params, ["#test"])

        self.wire.clear()
        self.channel.gg.icon = None
        self.protocol.cmd_channel_metadata("#test", self.channel, guild_icon=True)
        frame = self.frames()[0]
        descriptor = rdircd.json.loads(frame.tags["+dickord/channel"])
        self.assertIsNone(descriptor["guild_icon_url"])
        self.assertEqual(descriptor["channel_name"], "test")
        self.assertIn("+dickord/guild-icon", frame.tags)
        self.assertEqual(frame.tags["+dickord/guild-icon"], "")

        self.wire.clear()
        dm = rdircd.adict(
            id="30", ct=rdircd.DiscordSession.c_chan_type.private, parent_id=None,
            gg=rdircd.adict(id=1, name="me", icon="abcd"), names=rdircd.adict(raw=""),
        )
        dm_map = rdircd.adict(dm=rdircd.adict(name="dm", topic="", cc=dm))
        dm_map.ø_online = False
        self.protocol.cmd_join("#dm", cm=dm_map)
        frame = [frame for frame in self.frames() if frame.cmd == "tagmsg"][0]
        self.assertEqual(
            frame.tags["+dickord/channel"],
            '{"v":1,"guild_id":null,"guild_name":null,'
            '"channel_id":"30","channel_type":1,"parent_id":null,'
            '"channel_name":"DM 30","guild_icon_url":null,"channel_icon_url":null}',
        )
        self.assertNotIn("+dickord/guild-icon", frame.tags)

    def test_repeated_join_only_refreshes_the_descriptor(self):
        self.protocol.bridge = self.bridge
        self.conf._irc_nick_sys = self.conf.irc_nick_sys
        self.bridge.server_host = "rdircd"
        syncs, replays = [], []
        self.bridge.cmd_chan_map_sync_tracking = lambda *_args, **_kwargs: syncs.append(True)
        self.bridge.cmd_chan_watch_replay = lambda *_args, **_kwargs: replays.append(True)
        self.bridge.cmd_chan_names = lambda *_args, **_kwargs: []
        channel_map = rdircd.adict(
            test=rdircd.adict(name="test", topic="", cc=self.channel),
        )
        channel_map.ø_online = False
        self.protocol.st_irc.chans.pop("test")
        self.protocol.cmd_join("#test", cm=channel_map)
        descriptor = [
            frame.tags["+dickord/channel"]
            for frame in self.frames() if frame.cmd == "tagmsg"
        ][0]

        self.wire.clear()
        syncs.clear()
        channel_map.ø_online = True
        self.protocol.st_irc.ts_watch = 123
        self.protocol.cmd_join("#test", cm=channel_map)

        frames = self.frames()
        self.assertEqual([frame.cmd for frame in frames], ["tagmsg"])
        self.assertEqual(frames[0].tags["+dickord/channel"], descriptor)
        self.assertNotIn("+dickord/guild-icon", frames[0].tags)
        self.assertEqual(syncs, [])
        self.assertEqual(replays, [])
        self.assertEqual(self.protocol.st_irc.ts_watch, 123)

    async def test_watch_silent_matches_watch_without_success_chatter(self):
        self.conf.watch = rdircd.adict_rev()
        self.conf.state_fwd = lambda *_args, **_kwargs: []
        saved, cursors, notices = [], [], []
        self.conf.update_file_section = lambda section, values: saved.append(
            (section, dict(values))
        )
        self.conf.state_watch = lambda channel_id, *_args: cursors.append(channel_id)
        self.bridge.irc_discord_info = lambda _name: rdircd.adict(
            cc=self.channel, gg=self.channel.gg,
        )
        self.protocol.cmd_msg_chan_sys = lambda _chan, text: notices.append(text)

        await self.bridge.irc_cmd_topic(self.protocol, "test", "log watch")
        interactive = dict(self.conf.watch), list(saved), list(cursors)
        self.assertEqual(notices, ["History watch/replay enabled for this channel"])

        self.conf.watch = rdircd.adict_rev()
        saved.clear()
        cursors.clear()
        notices.clear()
        await self.bridge.irc_cmd_topic(self.protocol, "test", "log watch-silent")
        self.assertEqual((dict(self.conf.watch), saved, cursors), interactive)
        self.assertEqual(notices, [])

        await self.bridge.irc_cmd_topic(self.protocol, "test", "log watch-silent")
        self.assertEqual((dict(self.conf.watch), saved, cursors), interactive)
        self.assertEqual(notices, [])

        self.conf.watch = rdircd.adict_rev()
        saved.clear()
        cursors.clear()
        self.conf.misc_conf_readonly = True
        await self.bridge.irc_cmd_topic(self.protocol, "test", "log watch-silent")
        self.assertEqual(saved, [])
        self.assertEqual(cursors, ["20"])
        self.assertEqual(notices, [
            "History watch/replay changes for channel will not be stored "
            "due to read-only configuration",
        ])

        notices.clear()
        self.bridge.irc_discord_info = lambda _name: None
        with self.assertRaises(rdircd.IRCBridgeSignal):
            await self.bridge.irc_cmd_topic(self.protocol, "test", "log watch-silent")
        self.assertEqual(notices, [
            "topic-cmd-error: Not a discord channel: #test",
        ])

    def test_channel_metadata_reemits_only_authoritative_changes(self):
        self.protocol.bridge = self.bridge
        self.conf._irc_nick_sys = self.conf.irc_nick_sys
        self.bridge.server_host = "rdircd"
        self.bridge.cmd_chan_map_sync_tracking = lambda *_args, **_kwargs: None
        self.bridge.cmd_chan_names = lambda *_args, **_kwargs: []
        self.protocol.st_irc.chans.pop("test")
        channel = rdircd.adict(name="test", topic="", cc=self.channel)
        channel_map = rdircd.adict(test=channel)
        channel_map.ø_online = False
        self.protocol.cmd_join("#test", cm=channel_map)

        self.wire.clear()
        self.protocol.cmd_chan_list_sync(channel_map)
        self.assertEqual(self.wire, [])

        self.channel.gg.name = "Renamed Guild"
        self.protocol.cmd_chan_list_sync(channel_map)
        frame = self.frames()[0]
        self.assertEqual(
            frame.tags["+dickord/channel"],
            '{"v":1,"guild_id":"10","guild_name":"Renamed Guild",'
            '"channel_id":"20","channel_type":0,"parent_id":null,'
            '"channel_name":"test","guild_icon_url":null,"channel_icon_url":null}',
        )

        self.wire.clear()
        self.protocol.cmd_chan_list_sync(channel_map)
        self.assertEqual(self.wire, [])

        self.channel.ct = rdircd.DiscordSession.c_chan_type.thread
        self.channel.parent_id = "40"
        self.protocol.cmd_chan_list_sync(channel_map)
        self.assertEqual(
            self.frames()[0].tags["+dickord/channel"],
            '{"v":1,"guild_id":"10","guild_name":"Renamed Guild",'
            '"channel_id":"20","channel_type":11,"parent_id":"40",'
            '"channel_name":"test","guild_icon_url":null,"channel_icon_url":null}',
        )

        self.wire.clear()
        self.channel.parent_id = "41"
        self.protocol.cmd_chan_list_sync(channel_map)
        self.assertIn('"parent_id":"41"', self.frames()[0].tags["+dickord/channel"])

        self.wire.clear()
        self.channel.names.raw = "release.notes_20 🛠️"
        self.protocol.cmd_chan_list_sync(channel_map)
        self.assertEqual(
            rdircd.json.loads(self.frames()[0].tags["+dickord/channel"])["channel_name"],
            "release.notes_20 🛠️",
        )

    def test_channel_json_preserves_supported_dm_and_thread_types(self):
        for channel_type in (1, 3, 18):
            with self.subTest(channel_type=channel_type):
                descriptor = rdircd.discord_channel_json(rdircd.adict(
                    id="30", ct=channel_type, parent_id=None,
                    gg=rdircd.adict(id=1, name="me"), names=rdircd.adict(raw=""),
                ), "7")
                self.assertEqual(rdircd.json.loads(descriptor), {
                    "v": 1, "guild_id": None, "guild_name": None,
                    "channel_id": "30", "channel_type": channel_type,
                    "parent_id": None, "channel_name": "DM 30", "guild_icon_url": None,
                    "channel_icon_url": None,
                })
                self.assertNotIn(": ", descriptor)
        for channel_type in (10, 11, 12):
            with self.subTest(channel_type=channel_type):
                descriptor = rdircd.discord_channel_json(rdircd.adict(
                    id="31", ct=channel_type, parent_id="20",
                    gg=rdircd.adict(id="10", name="Guild"),
                    names=rdircd.adict(raw="release.notes_20"),
                ), "7")
                self.assertEqual(rdircd.json.loads(descriptor), {
                    "v": 1, "guild_id": "10", "guild_name": "Guild",
                    "channel_id": "31", "channel_type": channel_type,
                    "parent_id": "20", "channel_name": "release.notes_20",
                    "guild_icon_url": None, "channel_icon_url": None,
                })

    def test_channel_names_survive_irc_aliases_voice_suffixes_and_thread_templates(self):
        self.conf.renames[("chan", "@31")] = "irc-release-alias"
        self.conf.irc_thread_chan_name_len = 4
        self.channel.gg.icon = "abcd"
        for channel_id, channel_type, raw_name in (
            ("31", 0, "release.notes_20"),
            ("32", 2, "🛠️ Voice.notes_20 + Q&A!"),
            ("33", 11, "🧵 release.notes_20 :: shipped!"),
        ):
            with self.subTest(channel_type=channel_type):
                channel = self.process_channel(
                    self.channel.gg, id=channel_id, type=channel_type,
                    name=raw_name, parent_id="20", icon="ef01",
                )
                irc_name = self.bridge.irc_name(channel.name)
                self.assertNotEqual(irc_name, raw_name)
                descriptor = rdircd.json.loads(rdircd.discord_channel_json(channel, "7"))
                self.assertEqual(descriptor["channel_name"], raw_name)
                self.assertEqual(
                    descriptor["guild_icon_url"],
                    "https://cdn.discordapp.com/icons/10/abcd.png?size=256",
                )
                self.assertIsNone(descriptor["channel_icon_url"])

    def test_direct_dm_name_excludes_self_and_ignores_irc_user_alias(self):
        del self.discord.user_name
        self.conf.renames[("user", "@8")] = "irc-user-alias"
        self.conf.irc_chan_private = "fixed.{id}"
        for recipient, expected in (
            ({"id": "8", "username": "alice_irc", "global_name": "Alice Smith 🦊"}, "Alice Smith 🦊"),
            ({"id": "8", "username": "alice_irc", "global_name": None}, "alice_irc"),
            ({"id": "8", "username": None, "global_name": None}, "8"),
        ):
            with self.subTest(recipient=recipient):
                channel = self.process_channel(
                    self.session.st_da.me, id="30", type=1, name="Not the recipient",
                    recipients=[self.session.st_da.user, recipient],
                )
                descriptor = rdircd.json.loads(rdircd.discord_channel_json(channel, "7"))
                self.assertEqual(descriptor["channel_name"], expected)
                self.assertIsNone(descriptor["guild_name"])
                self.assertIsNone(descriptor["guild_icon_url"])

    def test_direct_dm_icon_uses_only_the_current_peer(self):
        del self.discord.user_name
        self.conf.irc_chan_private = "fixed.{id}"
        self.conf._irc_nick_sys = self.conf.irc_nick_sys
        self.protocol.bridge = self.bridge
        self.session.st_da.user.update(
            global_name="Aardvark", avatar="ffff", discriminator="0",
        )
        peer = {
            "id": "8", "username": "zed", "global_name": "Zed",
            "avatar": "a_ab12", "discriminator": "0",
        }
        channel = self.process_channel(
            self.session.st_da.me, id="30", type=1, name=None, recipients=[peer], icon="abcd",
        )
        self.protocol.cmd_channel_metadata("#test", channel)
        self.assertEqual(
            rdircd.json.loads(self.frames()[0].tags["+dickord/channel"])[
                "channel_icon_url"
            ],
            "https://cdn.discordapp.com/avatars/8/a_ab12.png?size=256",
        )

        self.wire.clear()
        channel = self.process_channel(
            self.session.st_da.me, id="30", type=1, topic="Only the topic changed",
        )
        self.protocol.cmd_channel_metadata("#test", channel)
        self.assertEqual(self.wire, [])

        channel = self.process_channel(
            self.session.st_da.me, id="30", type=1, recipients=[{
                "id": "8", "username": "zed", "global_name": "Zed", "avatar": None,
            }],
        )
        self.protocol.cmd_channel_metadata("#test", channel)
        self.assertIsNone(
            rdircd.json.loads(self.frames()[0].tags["+dickord/channel"])[
                "channel_icon_url"
            ],
        )
        self.wire.clear()
        self.protocol.cmd_channel_metadata("#test", channel)
        self.assertEqual(self.wire, [])

    def test_group_dm_names_keep_named_title_and_sort_raw_recipients(self):
        del self.discord.user_name
        self.conf.irc_chan_private = "fixed.{id}"
        recipients = [
            {"id": "10", "username": "alias10", "global_name": "ALICE"},
            {"id": "9", "username": "alias9", "global_name": "alice"},
            {"id": "31", "username": "alias31", "global_name": "SSam"},
            {"id": "30", "username": "alias30", "global_name": "ßam"},
            {"id": "8", "username": "bob", "avatar": "ab12"},
            self.session.st_da.user,
        ]
        for channel_type in (3, 18):
            for name, expected in (
                ("  Team.release_20 🛠️!  ", "  Team.release_20 🛠️!  "),
                ("  ", "alice, ALICE, bob, ßam, SSam"),
            ):
                with self.subTest(channel_type=channel_type, name=name):
                    channel = self.process_channel(
                        self.session.st_da.me, id="30", type=channel_type,
                        name=name, recipients=recipients, icon="a_ab12",
                    )
                    descriptor = rdircd.json.loads(rdircd.discord_channel_json(channel, "7"))
                    self.assertEqual(descriptor["channel_name"], expected)
                    self.assertEqual(
                        descriptor["channel_icon_url"],
                        "https://cdn.discordapp.com/channel-icons/30/a_ab12.png?size=256"
                        if channel_type == 3 else None,
                    )
                    recipients.reverse()

    async def test_group_icon_events_and_repeated_joins_refresh_metadata_without_chatter(self):
        self.conf.irc_chan_private = "fixed.{id}"
        self.conf._irc_nick_sys = self.conf.irc_nick_sys
        self.conf._irc_topic_hide_tags = set()
        self.conf.watch = rdircd.adict_rev()
        guild = self.session.st_da.me
        guild.update(kh="me", ts_joined=0)
        self.session.st_da.guilds = rdircd.adict({1: guild})
        channel = self.process_channel(
            guild, id="30", type=3, name="Team release", icon="abcd",
            recipients=[{"id": "8", "username": "alice", "avatar": "ffff"}],
        )
        self.bridge.server_host = "rdircd"
        self.bridge.server_ts = rdircd.dt.datetime.fromtimestamp(0, rdircd.dt.timezone.utc)
        self.bridge.conn_id = 1
        self.bridge.irc_conns = {"bridge": self.protocol}
        self.bridge.irc_chans_sys = rdircd.adict()
        self.bridge.st_br.update(
            chan_map=None, uid={}, gid_mon_chan={}, gid_nc_chan={}, gid_vc_chan={},
        )
        self.discord.st_eris.online = True
        self.protocol.bridge = self.bridge
        scheduled = []
        self.bridge.cmd_delay = lambda _delay, callback: scheduled.append(callback)
        channel_map = self.bridge.cmd_chan_map()
        name = self.bridge.st_br.did_chan[channel.did]
        self.protocol.st_irc.chans = {
            name: rdircd.adict(topic=channel_map[name].topic, cc=channel),
        }
        self.protocol.st_irc.ts_watch = 123
        self.protocol.cmd_channel_metadata(name, channel)
        expected = rdircd.json.loads(self.frames()[0].tags["+dickord/channel"])
        initial_icon = "https://cdn.discordapp.com/channel-icons/30/abcd.png?size=256"
        replaced_icon = "https://cdn.discordapp.com/channel-icons/30/ef01.png?size=256"
        self.assertEqual(expected["channel_icon_url"], initial_icon)
        scheduled.clear()

        for fields, icon_url, changed in (
            ({}, initial_icon, False),
            ({"icon": "ef01"}, replaced_icon, True),
            ({"icon": "ef01"}, replaced_icon, False),
            ({}, replaced_icon, False),
            ({"icon": None}, None, True),
            ({}, None, False),
        ):
            with self.subTest(fields=fields, icon_url=icon_url):
                self.wire.clear()
                self.session.op_ev_chans(1, rdircd.adict(id="30", type=3, **fields))
                while scheduled:
                    await scheduled.pop(0)()
                expected["channel_icon_url"] = icon_url
                frames = self.frames()
                self.assertEqual([frame.cmd for frame in frames], ["tagmsg"] if changed else [])
                if changed:
                    self.assertEqual(frames[0].params, [f"#{name}"])
                    self.assertEqual(
                        rdircd.json.loads(frames[0].tags["+dickord/channel"]), expected,
                    )

                self.wire.clear()
                self.protocol.cmd_join(f"#{name}", cm=self.bridge.cmd_chan_map())
                frames = self.frames()
                self.assertEqual([frame.cmd for frame in frames], ["tagmsg"])
                self.assertEqual(
                    rdircd.json.loads(frames[0].tags["+dickord/channel"]), expected,
                )
                self.assertEqual(self.protocol.st_irc.ts_watch, 123)

    def test_only_derived_group_title_is_truncated_by_unicode_code_points(self):
        del self.discord.user_name
        self.conf.irc_chan_private = "fixed.{id}"
        for width in (49, 50):
            first, second = "😀" * width, "🦊" * width
            title = f"{first}, {second}"
            channel = self.process_channel(
                self.session.st_da.me, id="30", type=3, name=None, recipients=[
                    {"id": "8", "global_name": first},
                    {"id": "9", "global_name": second},
                ],
            )
            descriptor = rdircd.json.loads(rdircd.discord_channel_json(channel, "7"))
            self.assertEqual(
                descriptor["channel_name"], title if width == 49 else title[:99] + "…",
            )
        channel = self.process_channel(
            self.session.st_da.me, id="30", type=3, name="🦊" * 100,
        )
        self.assertEqual(
            rdircd.json.loads(rdircd.discord_channel_json(channel, "7"))["channel_name"],
            "🦊" * 100,
        )

    def test_partial_channel_payloads_preserve_names_and_recipients_until_present(self):
        del self.discord.user_name
        self.conf.irc_chan_private = "fixed.{chat_name}.{id}"
        guild_channel = self.process_channel(
            self.channel.gg, id="31", type=0, name="release.notes_20 🛠️",
        )
        guild_channel = self.process_channel(
            self.channel.gg, id="31", type=0, topic="Only the topic changed",
        )
        self.assertEqual(
            rdircd.json.loads(rdircd.discord_channel_json(guild_channel, "7"))["channel_name"],
            "release.notes_20 🛠️",
        )
        channel = self.process_channel(
            self.session.st_da.me, id="30", type=3, name="Named group 🦊",
            recipients=[{"id": "8", "username": "alice", "global_name": "Alice Smith"}],
        )
        channel = self.process_channel(
            self.session.st_da.me, id="30", type=3, topic="Only the topic changed",
        )
        self.assertEqual(
            rdircd.json.loads(rdircd.discord_channel_json(channel, "7"))["channel_name"],
            "Named group 🦊",
        )
        for fields, expected in (
            ({"name": None}, "Alice Smith"),
            ({"recipients": [{"id": "9", "username": "Bob"}]}, "Bob"),
            ({"recipients": []}, "DM 30"),
            ({"name": "Renamed.group_20 🛠️"}, "Renamed.group_20 🛠️"),
        ):
            channel = self.process_channel(self.session.st_da.me, id="30", type=3, **fields)
            self.assertEqual(
                rdircd.json.loads(rdircd.discord_channel_json(channel, "7"))["channel_name"],
                expected,
            )

    def test_recipient_events_refresh_descriptors_without_reemitting_unchanged_values(self):
        del self.discord.user_name
        self.conf.irc_chan_private = "fixed.{id}"
        self.conf._irc_nick_sys = self.conf.irc_nick_sys
        self.bridge.server_host = "rdircd"
        self.bridge.irc_conns = {"bridge": self.protocol}
        self.protocol.bridge = self.bridge
        self.discord.cmd_msg_recv = lambda *_args, **_kwargs: None
        payload = dict(
            id="30", type=3, name=None, recipients=[{"id": "8", "username": "Bob"}],
        )
        channel = self.process_channel(self.session.st_da.me, **payload)
        self.protocol.st_irc.chans["test"].cc = channel
        self.protocol.cmd_channel_metadata("#test", channel)
        self.wire.clear()

        event = rdircd.adict(
            channel_id="30", user={"id": "9", "username": "alice", "global_name": "Alice Smith"},
        )
        self.session.op_ev_recipient(event, "add")
        self.assertEqual([frame.cmd for frame in self.frames()], ["tagmsg"])
        self.assertEqual(
            rdircd.json.loads(self.frames()[0].tags["+dickord/channel"])["channel_name"],
            "Alice Smith, Bob",
        )
        self.wire.clear()
        self.session.op_ev_recipient(event, "add")
        self.assertEqual(self.wire, [])

        self.session.op_ev_recipient(event, "remove")
        self.assertEqual(
            rdircd.json.loads(self.frames()[0].tags["+dickord/channel"])["channel_name"], "Bob",
        )
        self.wire.clear()
        self.session.op_ev_recipient(event, "remove")
        self.assertEqual(self.wire, [])

        self.session.op_ev_recipient(event, "add")
        channel = self.process_channel(self.session.st_da.me, **payload)
        self.protocol.cmd_channel_metadata("#test", channel)
        self.assertEqual(
            rdircd.json.loads(self.frames()[-1].tags["+dickord/channel"])["channel_name"], "Bob",
        )

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
        avatar = "https://cdn.discordapp.com/avatars/7/abcd.png?size=256"
        self.conf.irc_prefix_pinned = "saved: "
        self.discord.conn_req = mock.AsyncMock(return_value=[
            self.message(
                urls=[url], pinned=True,
                author={
                    "id": "7", "username": "owner", "avatar": "abcd",
                    "discriminator": "0",
                },
            )
        ])
        history = await self.discord.cmd_history(self.channel, 100, hwm=2)
        message = history.messages[0]
        self.assertEqual(message.discord_avatar, avatar)
        for _ in range(2):
            self.bridge.cmd_msg_discord(
                self.channel, message.nick, "[history] " + message.line,
                tags=message.tags, conn=self.protocol,
                discord_msg_id=message.msg_id, discord_user_id=message.discord_user_id,
                discord_avatar=message.discord_avatar,
                discord_reply_msg_id=message.discord_reply_msg_id,
                discord_self=message.discord_self,
            )
        self.assertEqual(self.wire[0], self.wire[1])
        self.assertEqual(len(self.wire), 2)
        frame = self.frames()[1]
        self.assertEqual(frame.tags["+dickord/avatar"], avatar)
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


class GuildIconUpdateTests(unittest.TestCase):
    def test_guild_updates_retain_icon_and_signal_only_changes(self):
        emitted = []
        session = rdircd.DiscordSession.__new__(rdircd.DiscordSession)
        session.log = Log()
        session.st_da = rdircd.adict(
            me=rdircd.adict(id=1),
            guilds={1: rdircd.adict(id=1)},
        )
        session.discord = rdircd.adict(
            bridge=rdircd.adict(
                uid=lambda *_args, **_kwargs: "guild",
                cmd_guild_icon=lambda guild: emitted.append(
                    rdircd.discord_guild_icon_url(guild)
                ),
            ),
            cmd_guild_event=lambda *_args, **_kwargs: None,
        )
        session.ws_req_guild_sync = lambda: None

        session.op_ev_guilds(
            rdircd.adict(id="10", name="Guild", icon="abcd"), init=True,
        )
        guild = session.st_da.guilds["10"]
        self.assertEqual(guild.icon, "abcd")
        self.assertEqual(emitted, [])

        session.op_ev_guilds(rdircd.adict(id="10", name="Guild"))
        session.op_ev_guilds(rdircd.adict(id="10", name="Guild", icon="abcd"))
        self.assertEqual(guild.icon, "abcd")
        self.assertEqual(emitted, [])

        session.op_ev_guilds(rdircd.adict(id="10", name="Guild", icon="ef01"))
        session.op_ev_guilds(rdircd.adict(id="10", name="Guild", icon=None))
        self.assertEqual(emitted, [
            "https://cdn.discordapp.com/icons/10/ef01.png?size=256",
            "",
        ])

        emitted.clear()
        session.op_ev_del_guild(rdircd.adict(id="10", unavailable=True))
        self.assertIn("10", session.st_da.guilds)
        self.assertEqual(emitted, [])
        session.op_ev_del_guild(rdircd.adict(id="10"))
        self.assertNotIn("10", session.st_da.guilds)
        self.assertEqual(emitted, [""])


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
    def upload_response(
            body, *, status=200, content_type="application/octet-stream",
            length=None, encoding=None):
        class Content:
            async def iter_chunked(self, _size):
                yield body

        headers = {"Content-Length": str(len(body) if length is None else length)}
        if encoding:
            headers["Content-Encoding"] = encoding
        response = rdircd.adict(
            status=status, headers=headers, content_type=content_type,
            content=Content(), released=False,
        )
        response.release = lambda: response.update(released=True)
        return response
    @staticmethod
    def opus_ogg(duration=2, pre_skip=312, eos=True):
        opus_head = (
            b"OpusHead\x01\x01"
            + pre_skip.to_bytes(2, "little")
            + (48000).to_bytes(4, "little")
            + b"\0\0\0"
        )
        opus_tags = b"OpusTags" + b"\0\0\0\0" + b"\0\0\0\0"
        audio = b"\xf8\xff\xfe"
        serial = (1).to_bytes(4, "little")
        def page(sequence, flags, granule, packet):
            return (
                b"OggS\x00" + bytes([flags]) + granule.to_bytes(8, "little")
                + serial + sequence.to_bytes(4, "little") + b"\0\0\0\0"
                + b"\x01" + bytes([len(packet)]) + packet
            )
        granule = pre_skip + duration * 48000
        return (
            page(0, 2, 0, opus_head)
            + page(1, 0, 0, opus_tags)
            + page(2, 4 if eos else 0, granule, audio)
        )

    class FormData:
        def __init__(self, **kwargs):
            self.fields = []
            self.kwargs = kwargs

        def add_field(self, name, value, **kwargs):
            self.fields.append((name, value, kwargs))

    def make_upload_discord(self, response):
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

    def test_privmsg_accepts_only_upload_one_and_preserves_voice_tags(self):
        protocol = rdircd.IRCProtocol.__new__(rdircd.IRCProtocol)
        protocol.cmd_msg_from_irc = mock.Mock()
        tags = {
            "+dickord/upload": "1",
            "+dickord/voice-duration": "1.25",
            "+dickord/voice-waveform": "AQI=",
        }
        protocol.recv_cmd_privmsg(rdircd.adict(
            params=["#test", "https://files.example/voice.ogg"], tags=tags,
        ))
        protocol.cmd_msg_from_irc.assert_called_once_with(
            "#test", "https://files.example/voice.ogg", from_self=True,
            ergo_msg_id=None, discord_reply_msg_id=None, upload=True,
            voice_duration="1.25", voice_waveform="AQI=",
        )

        protocol.cmd_msg_from_irc.reset_mock()
        tags["+dickord/upload"] = "yes"
        protocol.recv_cmd_privmsg(rdircd.adict(
            params=["#test", "https://files.example/voice.ogg"], tags=tags,
        ))
        self.assertFalse(protocol.cmd_msg_from_irc.call_args.kwargs["upload"])

    async def test_generic_upload_posts_attachment_only_multipart(self):
        image = b"\x89PNG\r\n\x1a\nimage"
        response = self.upload_response(image, content_type="image/png")
        discord, channel, request = self.make_upload_discord(response)
        url = "https://files.example/files/my%20photo.png"

        with mock.patch.object(rdircd.aiohttp, "FormData", self.FormData):
            result = await discord.cmd_msg_send_upload(
                channel, url, reply_msg_id="456",
            )

        self.assertEqual(result, "999")
        request.assert_awaited_once_with(
            "get", url, allow_redirects=False, auto_decompress=False,
            headers={"Accept-Encoding": "identity"},
        )
        post = discord.conn_req.await_args
        self.assertEqual(post.args, ("channels/20/messages",))
        self.assertEqual(post.kwargs["m"], "post")
        with mock.patch.object(rdircd.aiohttp, "FormData", self.FormData):
            form = post.kwargs["data_factory"]()
        self.assertNotIn("data", post.kwargs)
        self.assertEqual(form.kwargs, {"quote_fields": False})
        self.assertEqual([field[0] for field in form.fields], ["payload_json", "files[0]"])
        payload = rdircd.json.loads(form.fields[0][1])
        self.assertNotIn("content", payload)
        self.assertEqual(payload["flags"], 0)
        self.assertEqual(payload["attachments"], [{"id": 0, "filename": "my_photo.png"}])
        self.assertEqual(payload["message_reference"]["message_id"], "456")
        self.assertEqual(form.fields[1], (
            "files[0]",
            image,
            {"filename": "my_photo.png", "content_type": "image/png"},
        ))
        self.assertEqual(channel.last_msg_sent, {"flake": "old", "line": "old"})
        self.assertTrue(response.released)

    async def test_android_ogg_without_eos_posts_computed_voice_multipart(self):
        audio = self.opus_ogg(duration=2, pre_skip=312, eos=False)
        response = self.upload_response(audio, content_type="application/ogg")
        discord, channel, request = self.make_upload_discord(response)
        url = "https://files.example/voice.ogg"

        with mock.patch.object(rdircd.aiohttp, "FormData", self.FormData):
            result = await discord.cmd_msg_send_upload(channel, url)

        self.assertEqual(result, "999")
        request.assert_awaited_once_with(
            "get", url, allow_redirects=False, auto_decompress=False,
            headers={"Accept-Encoding": "identity"},
        )
        with mock.patch.object(rdircd.aiohttp, "FormData", self.FormData):
            form = discord.conn_req.await_args.kwargs["data_factory"]()
        payload = rdircd.json.loads(form.fields[0][1])
        self.assertEqual(payload["flags"], 8192)
        self.assertNotIn("content", payload)
        self.assertEqual(payload["attachments"], [{
            "id": 0,
            "filename": "voice.ogg",
            "duration_secs": 2.0,
            "waveform": "AA==",
        }])
        self.assertEqual(form.fields[1], (
            "files[0]",
            audio,
            {"filename": "voice.ogg", "content_type": "audio/ogg"},
        ))
        self.assertEqual(channel.last_msg_sent, {"flake": "old", "line": "old"})
        self.assertTrue(response.released)

    async def test_explicit_voice_metadata_overrides_ogg_values(self):
        response = self.upload_response(self.opus_ogg(), content_type="audio/ogg")
        discord, channel, _request = self.make_upload_discord(response)
        with mock.patch.object(rdircd.aiohttp, "FormData", self.FormData):
            await discord.cmd_msg_send_upload(
                channel, "https://files.example/voice.ogg", "1.25", "AQI=",
            )
        with mock.patch.object(rdircd.aiohttp, "FormData", self.FormData):
            form = discord.conn_req.await_args.kwargs["data_factory"]()
        payload = rdircd.json.loads(form.fields[0][1])
        self.assertEqual(payload["attachments"][0]["duration_secs"], 1.25)
        self.assertEqual(payload["attachments"][0]["waveform"], "AQI=")

    async def test_invalid_metadata_and_downloads_never_post(self):
        audio = self.opus_ogg()
        for duration, waveform in [("0", "AQI="), ("1", "not-base64")]:
            with self.subTest(duration=duration, waveform=waveform):
                discord, channel, request = self.make_upload_discord(
                    self.upload_response(audio, content_type="audio/ogg"),
                )
                with self.assertRaises(rdircd.IRCBridgeSignal):
                    await discord.cmd_msg_send_upload(
                        channel, "https://files.example/voice.ogg", duration, waveform,
                    )
                request.assert_not_awaited()
                discord.conn_req.assert_not_awaited()

        for response in [
            self.upload_response(audio, status=302, content_type="audio/ogg"),
            self.upload_response(b"not an ogg opus file", content_type="audio/ogg"),
            self.upload_response(audio, content_type="audio/ogg", length=25 * 2**20 + 1),
            self.upload_response(audio, content_type="audio/ogg", encoding="gzip"),
        ]:
            with self.subTest(status=response.status, content_type=response.content_type):
                discord, channel, _request = self.make_upload_discord(response)
                with self.assertRaises(rdircd.IRCBridgeSignal):
                    await discord.cmd_msg_send_upload(
                        channel, "https://files.example/voice.ogg",
                    )
                discord.conn_req.assert_not_awaited()

    async def test_queue_routes_uploads_and_keeps_untagged_urls_as_text(self):
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
            cmd_msg_send_upload=mock.AsyncMock(side_effect=["100", "101"]),
            cmd_msg_send=mock.AsyncMock(return_value="102"),
        )
        conn = rdircd.adict(
            chan_name=lambda _chan: "test",
            send=mock.Mock(),
            cmd_msg_chan_sys=mock.Mock(),
        )
        url = "https://files.example/voice.ogg"
        bridge.irc_msg(conn, "#test", url, ergo_msg_id="upload-ergo", upload=True)
        bridge.irc_msg(
            conn, "#test", url, ergo_msg_id="voice-ergo",
            voice_duration="1", voice_waveform="AQI=",
        )
        bridge.irc_msg(conn, "#test", url, ergo_msg_id="text-ergo")
        bridge.irc_msg_queue.put_nowait(StopIteration)

        await bridge.irc_msg_queue_proc()

        self.assertEqual(bridge.discord.cmd_msg_send_upload.await_args_list, [
            mock.call("channel", url, None, None, reply_msg_id=None),
            mock.call("channel", url, "1", "AQI=", reply_msg_id=None),
        ])
        bridge.discord.cmd_msg_send.assert_awaited_once_with(
            "channel", url, reply_msg_id=None,
        )
        self.assertEqual(conn.send.call_count, 3)


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

    async def test_request_data_factory_rebuilds_body_after_429(self):
        session = self.make_session()
        responses = [
            Response(status=429, body={"retry_after": 0.001}),
            Response(body={"id": "7"}),
        ]
        bodies = []

        class HTTP:
            async def request(self, *_args, **kwargs):
                bodies.append(kwargs["data"])
                return responses.pop(0)

        session.ws = rdircd.adict(http=HTTP())
        factory = mock.Mock(side_effect=[object(), object()])
        result = await session.req(
            "channels/123/messages", m="post", auth=False, data_factory=factory,
        )
        self.assertEqual(result, {"id": "7"})
        self.assertEqual(factory.call_count, 2)
        self.assertIsNot(bodies[0], bodies[1])


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
