// A log view backed by an EventSource. Every open clears the box and takes
// the server's replay, so a reconnect never shows a line twice. A reply that
// is not a stream (a 502, or the sign-in page) makes the browser give up; this
// helper then retries with backoff for five minutes, keeping the old log on
// screen while it does.
window.outhaulLogStream = function (opts) {
  var el = document.getElementById(opts.log);
  var indicator = document.getElementById(opts.indicator);
  var received = 0;
  var es = null;
  var retryTimer = null;
  var retryDelay = 2000;
  var retrySince = 0;
  var finished = false;

  function setIndicator(text) {
    if (indicator) { indicator.textContent = text; }
  }

  function clearBox() {
    el.textContent = '';
    received = 0;
  }

  function connect() {
    if (retryTimer) {
      clearTimeout(retryTimer);
      retryTimer = null;
    }
    if (es) { es.close(); }
    finished = false;

    es = new EventSource(opts.url());
    es.onopen = function () {
      clearBox();
      retryDelay = 2000;
      retrySince = 0;
      setIndicator('streaming…');
    };
    es.addEventListener('log', function (e) {
      var atBottom = (el.scrollTop + el.clientHeight) >= (el.scrollHeight - 4);
      el.textContent += e.data + '\n';
      if (atBottom) { el.scrollTop = el.scrollHeight; }
      received++;
    });
    es.addEventListener('err', function (e) {
      finished = true;
      es.close();
      el.textContent = e.data;
      setIndicator('unavailable');
    });
    es.addEventListener('done', function () {
      finished = true;
      es.close();
      setIndicator(opts.doneText || 'complete');
      if (opts.onDone) { opts.onDone(received); }
    });
    es.onerror = function () {
      if (finished) { return; }
      if (es.readyState === EventSource.CONNECTING) {
        setIndicator('reconnecting…');
        return;
      }
      if (!retrySince) { retrySince = Date.now(); }
      if (Date.now() - retrySince >= 5 * 60 * 1000) {
        setIndicator('disconnected. Reload the page to try again.');
        return;
      }
      setIndicator('reconnecting…');
      retryTimer = setTimeout(function () {
        retryTimer = null;
        connect();
      }, retryDelay);
      retryDelay = Math.min(retryDelay * 2, 30000);
    };
  }

  function open() {
    if (retryTimer) {
      clearTimeout(retryTimer);
      retryTimer = null;
    }
    if (es) { es.close(); }
    clearBox();
    retryDelay = 2000;
    retrySince = 0;
    setIndicator('connecting…');
    connect();
  }

  open();
  return { reopen: open };
};
