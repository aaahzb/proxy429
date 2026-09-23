// lang.go — web console UI language: follows the OS language by default (English
// when undetectable); the page top bar allows manual switching. The choice is stored
// in program-settings.txt under the config directory (program settings stay separate
// from routing config, never written into config*.json) and is inherited on every
// launch until changed again (choosing "follow system" deletes the key from
// program-settings.txt and restores auto-detection).
// A legacy ui_lang key in config files is migrated once at startup by resolveProgramUILang.
package main

import (
	"sync/atomic"
)

// uiLang is the active UI language ("zh"/"en"); written only via applyUILang, read by page handlers.
var uiLang atomic.Value

// applyUILang applies the configured language choice; an empty value falls back to OS language detection.
func applyUILang(cfgLang string) {
	if cfgLang == "zh" || cfgLang == "en" {
		uiLang.Store(cfgLang)
		return
	}
	uiLang.Store(detectSystemLang())
}

// currentUILang returns the active UI language (always initialized at main startup; English as fallback).
func currentUILang() string {
	if v := uiLang.Load(); v != nil {
		return v.(string)
	}
	return "en"
}
