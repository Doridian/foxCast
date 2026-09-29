//go:build !nogui

package gui

/*
#cgo CXXFLAGS: -std=c++17 -fPIC
#cgo pkg-config: Qt6Gui
#cgo LDFLAGS: -lLayerShellQtInterface
#include "layershell.h"
*/
import "C"

import (
	qt "github.com/mappu/miqt/qt6"
)

// layerShellPlace positions window as a layer-shell surface (Wayland).
func layerShellPlace(window *qt.QWindow, screen *qt.QScreen, p placement) {
	C.foxcast_layer_shell_place(window.UnsafePointer(), screen.UnsafePointer(), C.int(p.anchors),
		C.int(p.left), C.int(p.top), 0, 0)
}
