# q2-deezer

Deezer for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod), as [OpenDeezer](https://github.com/Cycl0o0/OpenDeezer) does Deezer: browse your Deezer library and Deezer's catalogue on the Q2, and what you pick is downloaded to the card, decrypted and tagged, lyrics included. Every song then plays from the card through the Q2's own player, as local music, with no Wi-Fi. Or switch to **Stream** mode, and a press plays the song straight away without keeping it. It needs a paid Deezer plan (Premium or HiFi) and a Q2 Pod build that supports card apps (its **System settings → Apps** lists Deezer).

## Install

**Over Wi-Fi:** on the Q2, open **System settings → Apps** and press **Deezer**.

**By hand:**

1. Download `q2-deezer-*.zip` from the [latest release](https://github.com/group-a-study/q2-deezer/releases/latest).
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.deezer/q2-deezer`, `.deezer/run`, `.deezer/q2app` and its icons `.deezer/icon.png` and `.deezer/icon-40.png`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

Deezer then has a row in **Streaming**, with its logo.

## Sign in

Open **Streaming → Deezer** and press **Sign In from Your Phone**: the Q2 shows a QR code, an address and a PIN. Scan the code with your phone (or open the address on a computer on the same Wi-Fi and type the PIN). The page explains how to copy the `arl` cookie from deezer.com in a computer's browser and has a box to paste it in. It is checked with Deezer (an account without a paid plan is refused), then saved as `.deezer/arl` on the card. Treat it like a password. **Account → Sign Out** removes it; your downloads stay.

## Use

Deezer's page has **Downloaded Songs**, **Liked Songs**, **Playlists** (Flow and Top Charts first, then yours), **Albums**, **Artists**, **Search Deezer**, **Downloads**, **Mode** (Download or Stream), **Quality** (FLAC, MP3 320 or MP3 128), **Organize Files**, **Account** and **Stop Deezer**.

- Press a song to download it. **Download All** downloads a whole list, and so does holding **Play/Pause** on a playlist, an album or Liked Songs.
- A song already on the card says so, and pressing it plays it from the card.
- Downloads carry on with the page closed, one song at a time, and wait by themselves while you're offline. The Q2 stays awake while a song downloads.
- Run **Library → Update Local Music** for your downloads to show in Artists, Albums and Coverflow too.
- Each song is tagged with its lyrics: time-synced ones as LRC (`[01:03.01]A line`), shown in time in Now Playing on Q2 Pod and by Navidrome and most players, else the plain text. Synced lyrics come from [LRCLIB](https://lrclib.net) when it has them, as Q2 Pod's own lyrics search finds them, since Deezer's light each line about 0.75 s before it's sung; otherwise Deezer's.
- A song Deezer doesn't license in your account's country (an account made in Turkey is TR wherever you use it) downloads from another release of it when Deezer has one, as Deezer's own apps do: Angus & Julia Stone's *Snow* is licensed only in Australia and New Zealand, so it comes from its international edition. With none, it fails saying so.

## Stream or download

**Mode: Download** (the default): a press downloads the song to `Deezer/`, as above.

**Mode: Stream**: a press plays the song, and the rest of its list after it, straight away. **Play All** plays a list from its start. Songs are fetched a couple ahead of the one playing into a hidden folder (`.deezer/stream`) and deleted as you go on, so nothing joins your library and the card doesn't fill up. Holding **Play/Pause** on a song still downloads it. Stream mode needs a Q2 Pod with stream support (group-a-study/q2-pod 1.4.2, test build V007i/V008s or later); the setting is saved in `.deezer/mode`.

## Organize Files

**On** (the default): each new song is filed by its album's artist, then its album's year and title, numbered in track order. The year comes first, so an artist's albums list oldest first wherever they are browsed by name (the Q2's Folder view, a computer):

    Deezer/Broadcast/2005 - Tender Buttons/01 - Tears in the Typing Pool [12345].flac
    Deezer/Broadcast/2005 - Tender Buttons/02 - Valerie [12346].flac
    Deezer/Various Artists/2019 - Some Compilation/2-07 - Song [23456].flac    (disc 2 of 2)

Navidrome (and Plex and Jellyfin) group your music by its tags (album, album artist, year), which every download carries, not by folder names, so this layout works with them as any other would.

A song downloaded from a playlist, Liked Songs or a search lands in its album's folder with its track number too: q2-deezer looks it up first. **Downloaded Songs** then lists your artists, each artist's albums oldest first (with the year beside each), and each album's songs in track order. A press plays the album from that song on.

**Off:** every new song goes straight into `Deezer/`, as `Title - Artist [12345].flac`, as OpenDeezer names them. **Downloaded Songs** lists them newest first.

The setting is saved in `.deezer/organize` and applies to new downloads. Songs already on the card stay where they are and are still recognised as downloaded. v3.3 renames the album folders v3.2 made (`Tender Buttons (2005)`) year first once, when it first starts; one made by v3.1, without its year, is renamed when that album's next song downloads, so an album stays in one folder. Run **Library → Update Local Music** afterwards.

## Update and remove

**System settings → Apps** shows **Update** when a newer release is out. By hand, replace `.deezer/q2-deezer`, `.deezer/run`, `.deezer/q2app`, `.deezer/icon.png` and `.deezer/icon-40.png`. Either way, `.deezer/arl` (your sign-in), `.deezer/quality`, `.deezer/organize`, `.deezer/mode` and `.deezer/queue.json` (songs waiting to download) are kept.

To remove it, press **Deezer** twice in **System settings → Apps**, or delete `.deezer`. Your songs in `Deezer/` stay.

## Build

    contrib/shanlingq2/build.sh

needs Go 1.22 or later and python3 (the dependencies are vendored), and leaves the card files in `target/shanlingq2/.deezer` and the release ZIP beside them. Pushing a tag `q2-vX.Y` that matches `cmd/q2-deezer/main.go`'s version builds and publishes the release (`.github/workflows/release.yml`). How it works: [contrib/shanlingq2/README](contrib/shanlingq2/README).

## License

[AGPL-3.0](LICENSE), as OpenDeezer, whose Deezer client (`internal/deezer`) this carries. The vendored libraries keep their own licenses (`vendor/`), and Mozilla's CA bundle (`internal/tlsroots/cacert.pem`) is MPL-2.0.
