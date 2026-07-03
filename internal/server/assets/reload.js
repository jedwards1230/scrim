(function () {
  try {
    var es = new EventSource("__SCRIM_EVENTS_URL__");
    es.onmessage = function () {
      location.reload();
    };
  } catch (e) {
    // EventSource unsupported or blocked; live reload just won't fire.
  }
})();
