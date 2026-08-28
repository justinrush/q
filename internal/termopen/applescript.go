package termopen

const openScript = `on run argv
    set missionDirectory to item 1 of argv
    set missionCommand to item 2 of argv

    tell application "Ghostty"
        activate
        set windowConfig to new surface configuration
        set initial working directory of windowConfig to missionDirectory
        set command of windowConfig to missionCommand
        new window with configuration windowConfig
    end tell
end run`

const raiseScript = `tell application "Ghostty" to activate`
