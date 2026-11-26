//go:build windows

package main

import (
	_ "embed"
	"image/color"
	"time"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
	"golang.org/x/sys/windows"
)

//go:embed timeout-background.jpg
var timeoutBackground []byte

//go:embed timeout-scare.png
var timeoutScareImage []byte

//go:embed timeout-scare.wav
var timeoutScareSound []byte

var timeoutKeyState = user32.NewProc("GetAsyncKeyState")
var timeoutPlaySound = windows.NewLazySystemDLL("winmm.dll").NewProc("PlaySoundW")

type timeoutCloseButton struct {
	widget.Button
	scare func()
}

func timeoutKeyHeld(key uintptr) bool {
	state, _, _ := timeoutKeyState.Call(key)
	return state&0x8000 != 0
}

var _ desktop.Mouseable = (*timeoutCloseButton)(nil)

func (b *timeoutCloseButton) MouseUp(*desktop.MouseEvent) {}

func (b *timeoutCloseButton) MouseDown(event *desktop.MouseEvent) {
	if event.Button&desktop.MouseButtonPrimary == 0 {
		return
	}
	const (
		vkControl     = 0x11
		vkRightButton = 0x02
	)
	if event.Modifier&fyne.KeyModifierControl != 0 || timeoutKeyHeld(vkControl) || timeoutKeyHeld(vkRightButton) {
		hideTimeUp()
		return
	}
	b.scare()
}

func buildTimeUpWindow(app fyne.App) fyne.Window {
	window := app.NewWindow(TimeUpTitle)
	background := canvas.NewImageFromResource(fyne.NewStaticResource("timeout-background.jpg", timeoutBackground))
	background.FillMode = canvas.ImageFillCover
	tint := canvas.NewRectangle(color.NRGBA{A: 110})

	title := canvas.NewText("TIME'S UP", color.NRGBA{R: 255, G: 112, B: 105, A: 255})
	title.TextSize = 84
	title.TextStyle = fyne.TextStyle{Bold: true}
	title.Alignment = fyne.TextAlignCenter

	scare := canvas.NewImageFromResource(fyne.NewStaticResource("timeout-scare.png", timeoutScareImage))
	scare.FillMode = canvas.ImageFillCover
	scare.Hide()

	lastScare := time.Time{}
	closeButton := &timeoutCloseButton{Button: widget.Button{Text: "Close", Importance: widget.HighImportance}}
	closeButton.ExtendBaseWidget(closeButton)
	closeButton.scare = func() {
		if time.Since(lastScare) < 2*time.Second {
			return
		}
		lastScare = time.Now()
		scare.Show()
		const asyncMemory = 0x0001 | 0x0004 | 0x0002
		if played, _, _ := timeoutPlaySound.Call(uintptr(unsafe.Pointer(&timeoutScareSound[0])), 0, asyncMemory); played == 0 {
			logf("timeout", "jump scare sound could not play")
		}
		time.AfterFunc(700*time.Millisecond, func() { fyne.Do(scare.Hide) })
	}
	alert := container.NewVBox(title, container.NewCenter(closeButton))
	frame := canvas.NewRectangle(color.Transparent)
	frame.StrokeColor = color.NRGBA{R: 255, G: 154, B: 109, A: 255}
	frame.StrokeWidth = 6
	window.SetContent(container.NewStack(background, tint, container.NewCenter(alert), scare, container.NewPadded(frame)))
	window.SetCloseIntercept(func() {})
	return window
}
