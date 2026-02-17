# fpm (Go Port)

A pure Go port of the [Flashpoint Manager](https://github.com/FlashpointProject/fpm), originally written in C#. This command-line tool manages components (downloading, updating, removing) for [Flashpoint Archive](https://flashpointarchive.org/) on Linux systems.

## Features

- **Single Binary**: No .NET runtime required.
- **Linux Native**: Handles file paths and permissions correctly for Linux environments.
- **Compatible**: Uses the same configuration files (`fpm.cfg`) and directory structures (`Components/`) as the Windows version, allowing for cross-platform data usage if needed.

## Installation

### Prerequisites
- Go 1.16 or higher

### Build
Save the source code as `main.go`.

```bash
go build -o fpm main.go
chmod +x fpm
