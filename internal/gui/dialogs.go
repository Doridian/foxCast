//go:build !nogui

package gui

import (
	"html"
	"path/filepath"

	qt "github.com/mappu/miqt/qt6"
)

// credentialText is the wording of a pairing prompt.
type credentialText struct {
	heading, body, placeholder string
	secret                     bool
}

func credentialWording(receiver string, kind CredentialKind) credentialText {
	name := html.EscapeString(receiver)
	switch kind {
	case CredentialPIN:
		return credentialText{
			heading:     "Enter the PIN shown on " + name,
			body:        "The receiver is displaying a pairing code on screen. foxCast remembers the pairing, so this is only needed once.",
			placeholder: "PIN",
		}
	case CredentialPassword:
		return credentialText{
			heading:     name + " requires a password",
			body:        "Enter the AirPlay password configured on the receiver. It is saved with the pairing so you are not asked again.",
			placeholder: "Password",
			secret:      true,
		}
	default:
		return credentialText{
			heading:     name + " requires a code",
			body:        "Enter the PIN shown on the receiver's screen, or the AirPlay password configured on it.",
			placeholder: "PIN or password",
			secret:      true,
		}
	}
}

// showCredentialDialog shows a non-blocking pairing prompt. done runs once
// with the entered value and whether the user confirmed. The returned
// function dismisses the dialog if it is still open.
func showCredentialDialog(parent *qt.QWidget, receiver string, kind CredentialKind, done func(value string, ok bool)) func() {
	wording := credentialWording(receiver, kind)
	d := qt.NewQDialog(parent)
	d.SetWindowTitle("Pair with " + receiver)
	d.SetMinimumWidth(400)
	layout := qt.NewQVBoxLayout(d.QWidget)

	top := qt.NewQHBoxLayout2()
	icon := qt.NewQLabel2()
	icon.SetPixmap(qt.QIcon_FromTheme("dialog-password").PixmapWithExtent(48))
	icon.SetAlignment(qt.AlignTop)
	top.AddWidget(icon.QWidget)
	text := qt.NewQLabel2()
	text.SetTextFormat(qt.RichText)
	text.SetWordWrap(true)
	text.SetText("<p><b>" + wording.heading + "</b></p><p>" + wording.body + "</p>")
	top.AddWidget2(text.QWidget, 1)
	layout.AddLayout(top.QLayout)

	edit := qt.NewQLineEdit(d.QWidget)
	edit.SetPlaceholderText(wording.placeholder)
	if wording.secret {
		edit.SetEchoMode(qt.QLineEdit__Password)
		reveal := edit.AddAction2(qt.QIcon_FromTheme("password-show-on"), qt.QLineEdit__TrailingPosition)
		reveal.SetToolTip("Show password")
		shown := false
		reveal.OnTriggered(func() {
			shown = !shown
			if shown {
				edit.SetEchoMode(qt.QLineEdit__Normal)
				reveal.SetIcon(qt.QIcon_FromTheme("password-show-off"))
				reveal.SetToolTip("Hide password")
			} else {
				edit.SetEchoMode(qt.QLineEdit__Password)
				reveal.SetIcon(qt.QIcon_FromTheme("password-show-on"))
				reveal.SetToolTip("Show password")
			}
		})
	} else {
		// A large, centred PIN field, digits only.
		font := qt.NewQFont5(edit.Font())
		font.SetPointSizeF(font.PointSizeF() * 2)
		edit.SetFont(font)
		edit.SetAlignment(qt.AlignCenter)
		edit.SetValidator(qt.NewQRegularExpressionValidator4(qt.NewQRegularExpression2(`[0-9]{0,8}`), edit.QObject).QValidator)
	}
	layout.AddWidget(edit.QWidget)

	buttons := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Ok | qt.QDialogButtonBox__Cancel)
	pair := buttons.Button(qt.QDialogButtonBox__Ok)
	pair.SetText("Pair")
	pair.SetIcon(qt.QIcon_FromTheme("network-connect"))
	pair.SetEnabled(false)
	edit.OnTextChanged(func(value string) { pair.SetEnabled(value != "") })
	buttons.OnAccepted(d.Accept)
	buttons.OnRejected(d.Reject)
	layout.AddWidget(buttons.QWidget)

	closed := false
	d.OnFinished(func(result int) {
		closed = true
		done(edit.Text(), result == int(qt.QDialog__Accepted))
		d.DeleteLater()
	})
	d.Show()
	d.Raise()
	d.ActivateWindow()
	edit.SetFocus()
	return func() {
		if !closed {
			d.Reject()
		}
	}
}

// askMedia asks for a URL or file to play on r, offering the clipboard's
// contents when they look like one.
func (a *app) askMedia(r Receiver) (string, bool) {
	d := qt.NewQDialog(a.dialogParent())
	defer d.DeleteLater()
	d.SetWindowTitle("Play on " + r.Name)
	d.SetMinimumWidth(520)
	layout := qt.NewQVBoxLayout(d.QWidget)

	top := qt.NewQHBoxLayout2()
	icon := qt.NewQLabel2()
	icon.SetPixmap(qt.QIcon_FromTheme("video-x-generic").PixmapWithExtent(48))
	icon.SetAlignment(qt.AlignTop)
	top.AddWidget(icon.QWidget)
	text := qt.NewQLabel2()
	text.SetTextFormat(qt.RichText)
	text.SetWordWrap(true)
	text.SetText("<p><b>Play a video on " + html.EscapeString(r.Name) + "</b></p>" +
		"<p>Paste a link (HLS, MP4, MKV, …) or choose a file on this computer. " +
		"Matroska (MKV/WebM) files are remuxed on the fly without re-encoding.</p>")
	top.AddWidget2(text.QWidget, 1)
	layout.AddLayout(top.QLayout)

	row := qt.NewQHBoxLayout2()
	edit := qt.NewQLineEdit(d.QWidget)
	edit.SetPlaceholderText("https://example.com/video.m3u8 or /path/to/video.mkv")
	edit.SetClearButtonEnabled(true)
	row.AddWidget2(edit.QWidget, 1)
	browse := qt.NewQPushButton4(qt.QIcon_FromTheme(iconFile), "Browse…")
	browse.OnClicked(func() {
		dir := a.lastDir
		if current := mediaLocation(edit.Text()); current != "" && filepath.IsAbs(current) {
			dir = filepath.Dir(current)
		}
		file := qt.QFileDialog_GetOpenFileName4(d.QWidget, "Choose a Video", dir, videoFileFilter)
		if file != "" {
			a.lastDir = filepath.Dir(file)
			edit.SetText(file)
		}
	})
	row.AddWidget(browse.QWidget)
	layout.AddLayout(row.QLayout)

	problem := qt.NewQLabel2()
	problem.SetEnabled(false)
	layout.AddWidget(problem.QWidget)

	buttons := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Ok | qt.QDialogButtonBox__Cancel)
	play := buttons.Button(qt.QDialogButtonBox__Ok)
	play.SetText("Play")
	play.SetIcon(qt.QIcon_FromTheme("media-playback-start"))
	validate := func(value string) {
		ok := mediaLocation(value) != ""
		play.SetEnabled(ok)
		switch {
		case ok || value == "":
			problem.SetText(" ")
		default:
			problem.SetText("Not a link or an existing file.")
		}
	}
	edit.OnTextChanged(validate)
	buttons.OnAccepted(d.Accept)
	buttons.OnRejected(d.Reject)
	layout.AddWidget(buttons.QWidget)

	if clip := mediaLocation(qt.QGuiApplication_Clipboard().Text()); clip != "" {
		edit.SetText(clip)
		edit.SelectAll()
	}
	validate(edit.Text())
	edit.SetFocus()

	if d.Exec() != int(qt.QDialog__Accepted) {
		return "", false
	}
	location := mediaLocation(edit.Text())
	return location, location != ""
}
