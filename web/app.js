(function () {
  "use strict";

  var keyEl = document.getElementById("key");
  var apiKeyEl = document.getElementById("apikey");
  var valueEl = document.getElementById("value");
  var resultEl = document.getElementById("result");
  var statusEl = document.getElementById("status");
  var counterEl = document.getElementById("counter");
  var copyEl = document.getElementById("copy");

  var SESSION_KEY = "kvp.apiKey";

  apiKeyEl.value = sessionStorage.getItem(SESSION_KEY) || "";
  apiKeyEl.addEventListener("input", function () {
    if (apiKeyEl.value) {
      sessionStorage.setItem(SESSION_KEY, apiKeyEl.value);
    } else {
      sessionStorage.removeItem(SESSION_KEY);
    }
  });

  function bytes(n) {
    if (n < 1024) {
      return n + " B";
    }
    if (n < 1048576) {
      return (n / 1024).toFixed(1) + " KB";
    }
    return (n / 1048576).toFixed(1) + " MB";
  }

  valueEl.addEventListener("input", function () {
    counterEl.textContent = bytes(valueEl.value.length);
  });

  function keyPath() {
    var k = (keyEl.value || "").trim();
    if (!k) {
      show("key required", "err", "HTTP 400");
      return null;
    }
    return k.charAt(0) === "/" ? k : "/" + k;
  }

  function show(text, cls, status) {
    resultEl.textContent = text;
    resultEl.className = "meta";
    if (cls === "ok") {
      resultEl.className = "ok";
    } else if (cls === "err") {
      resultEl.className = "err";
    }
    statusEl.textContent = status || "";
    statusEl.className = "status" + (cls === "ok" ? " ok" : cls === "err" ? " err" : "");
  }

  function req(method, path, body, onDone) {
    var xhr = new XMLHttpRequest();
    xhr.open(method, path, true);
    if (apiKeyEl.value) {
      xhr.setRequestHeader("X-API-Key", apiKeyEl.value);
    }
    xhr.onreadystatechange = function () {
      if (xhr.readyState === 4) {
        onDone(xhr.status, xhr.responseText);
      }
    };
    xhr.send(body);
  }

  function errorMessage(status, text) {
    var msg = text;
    try {
      var parsed = JSON.parse(text);
      if (parsed && parsed.error) {
        msg = parsed.error;
      }
    } catch (e) {
      msg = text;
    }
    if (status === 401) {
      msg = "unauthorized \u2014 enter the server API key (KVP_API_KEY) above";
    }
    return msg;
  }

  document.getElementById("put").addEventListener("click", function () {
    var path = keyPath();
    if (!path) {
      return;
    }
    var body = valueEl.value;
    req("POST", path, body, function (status, text) {
      if (status === 200 || status === 201) {
        show("stored " + path + " (" + bytes(body.length) + ")", "ok", "HTTP " + status);
      } else {
        show(errorMessage(status, text), "err", "HTTP " + status);
      }
    });
  });

  document.getElementById("get").addEventListener("click", function () {
    var path = keyPath();
    if (!path) {
      return;
    }
    req("GET", path, null, function (status, text) {
      if (status === 200) {
        show(text, "ok", "HTTP " + status + " \u00b7 " + bytes(text.length));
      } else {
        show(errorMessage(status, text), "err", "HTTP " + status);
      }
    });
  });

  keyEl.addEventListener("keydown", function (e) {
    if (e.key === "Enter") {
      document.getElementById("get").click();
    }
  });

  copyEl.addEventListener("click", function () {
    var text = resultEl.textContent;
    if (!text || text === "Ready.") {
      return;
    }
    navigator.clipboard.writeText(text).then(function () {
      copyEl.textContent = "Copied";
      setTimeout(function () {
        copyEl.textContent = "Copy";
      }, 1200);
    });
  });
})();
