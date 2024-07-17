# InstaFix Remote Scraper

To use this you need to have a LTE phone and [adb](https://developer.android.com/studio/command-line/adb) installed.

## Usage

1. Connect your phone to your computer via USB
2. Make sure your phone is in developer mode and able to connect to ADB
3. Build the app with `GOOS=linux GOARCH=arm64 go build`
4. Push the app to your phone with `adb push remotescraper /sdcard`
5. Run the app with `adb shell` then `nohup /sdcard/remotescraper &`
6. Remote scraper will be running on `localhost:3001`

You can connect your phone to your InstaFix server by using [frp](https://github.com/fatedier/frp).
