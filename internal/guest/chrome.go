package guest

import "os"

// Chrome 15x opens a "Google Chrome and ChromeOS Additional Terms of Service"
// dialog on the first start of every new profile, which blocks agents that
// open a page on the desktop (Boat's computer-use example). v3 images ship
// /opt/google/chrome/initial_preferences with require_eula false
// (images/scripts/full.sh); for older images the agent writes the same file
// at boot when Chrome is installed and the file is missing.
const chromeInitialPrefs = `{"distribution": {"require_eula": false, "skip_first_run_ui": true, "suppress_first_run_default_browser_prompt": true}}` + "\n"

var chromeDir = "/opt/google/chrome"

func (a *Agent) ensureChromeFirstRun() {
	if a.unpriv {
		return
	}
	if _, err := os.Stat(chromeDir + "/chrome"); err != nil {
		return
	}
	p := chromeDir + "/initial_preferences"
	if _, err := os.Stat(p); err == nil {
		return // the image's (or the admin's) own file wins
	}
	if err := a.writeFileAtomic(p, []byte(chromeInitialPrefs), 0o644, false); err != nil {
		a.log.Printf("chrome initial_preferences: %v", err)
	}
}
