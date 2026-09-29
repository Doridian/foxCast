//go:build gui

#include <LayerShellQt/window.h>
#include <QMargins>
#include <QScreen>
#include <QWindow>

#include "layershell.h"

void foxcast_layer_shell_place(void *window, void *screen, int anchors, int left, int top, int right, int bottom)
{
    auto *layer = LayerShellQt::Window::get(static_cast<QWindow *>(window));
    layer->setScope(QStringLiteral("foxcast"));
    layer->setLayer(LayerShellQt::Window::LayerTop);
    layer->setKeyboardInteractivity(LayerShellQt::Window::KeyboardInteractivityOnDemand);
    layer->setActivateOnShow(true);
    layer->setExclusiveZone(0);
    if (screen) {
        layer->setWantsToBeOnActiveScreen(false);
        layer->setScreen(static_cast<QScreen *>(screen));
    }
    layer->setAnchors(LayerShellQt::Window::Anchors(anchors));
    layer->setMargins(QMargins(left, top, right, bottom));
}
