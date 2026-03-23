# foxCast

foxCast is a project that aims to implement the current version of Apple's AirPlay protocol on the sender end.

In the end, it should be easy to stream any type of content to an Apple TV or other AirPlay receiver.

## MVP

- Can connect to AppleTV devices
- Can stream video and audio given a local file
- Runs on Linux
- Impelemented in Go or Rust

## Sources

- https://github.com/FDH2/UxPlay - implements a receiver for current AirPlay
- https://github.com/openairplay/open-airplay - collection of misc AirPlay libraries, some might be outdated

## Later goals

- Can connect to any type of AirPlay receiver
- Controls for streamed video files
- Ability to take in data via xdg-desktop-portal for app and window streaming
- Runs on Linux and Windows
