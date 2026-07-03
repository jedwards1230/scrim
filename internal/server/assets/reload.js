(function () {
  try {
    var es = new EventSource("__SCRIM_EVENTS_URL__");
    // The server emits a named "reload" event (see handlers_sse.go); browsers
    // only route named SSE events to listeners registered for that event
    // name, not to onmessage (which only fires for default/unnamed events).
    es.addEventListener("reload", function () {
      location.reload();
    });
  } catch (e) {
    // EventSource unsupported or blocked; live reload just won't fire.
  }
})();
