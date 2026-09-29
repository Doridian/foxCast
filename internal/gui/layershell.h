// C interface to LayerShellQt for the foxCast popup.
#ifndef FOXCAST_LAYERSHELL_H
#define FOXCAST_LAYERSHELL_H

#ifdef __cplusplus
extern "C" {
#endif

// foxcast_layer_shell_place makes window (a QWindow* not yet shown for the
// first time) a layer-shell surface on screen (a QScreen*, may be NULL),
// anchored and inset as given. Later calls, made while the window is hidden,
// move it.
void foxcast_layer_shell_place(void *window, void *screen, int anchors, int left, int top, int right, int bottom);

#ifdef __cplusplus
}
#endif

#endif
