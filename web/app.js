(function () {
  "use strict";

  var keyEl = document.getElementById("key");
  var apiKeyEl = document.getElementById("apikey");
  var valueEl = document.getElementById("value");
  var resultEl = document.getElementById("result");

  var SESSION_KEY = "kvp.apiKey";

  apiKeyEl.value = sessionStorage.getItem(SESSION_KEY) || "";
  apiKeyEl.addEventListener("input", function () {
    if (apiKeyEl.value) {
      sessionStorage.setItem(SESSION_KEY, apiKeyEl.value);
    } else {
      sessionStorage.removeItem(SESSION_KEY);
    }
  });

  function keyPath() {
    var k = (keyEl.value || "").trim();
    if (!k) {
      show("key required", true);
      return null;
    }
    if (k.charAt(0) !== "/") {
      k = "/" + k;
    }
    return k;
  }

  function show(text, isError) {
    resultEl.textContent = text;
    resultEl.className = isError ? "err" : "ok";
  }

  function req(method, path, body, onDone) {
    var xhr = new XMLHttpRequest();
    xhr.open(method, path, true);
    if (apiKeyEl.value) {
      xhr.setRequestHeader("X-API-Key", apiKeyEl.value);
    }
    xhr.onreadystatechange = function () {
      if (xhr.readyState !== 4) {
        return;
      }
      onDone(xhr.status, xhr.responseText);
    };
    xhr.send(body);
  }

  function renderStatus(status) {
    return status === 200 || status === 201 ? "OK (" + status + ")" : "HTTP " + status;
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
      msg = "unauthorized — enter the server API key (KVP_API_KEY) above";
    }
    return renderStatus(status) + ": " + msg;
  }

  document.getElementById("put").addEventListener("click", function () {
    var path = keyPath();
    if (!path) {
      return;
    }
    var body = valueEl.value;
    req("POST", path, body, function (status, text) {
      if (status === 200 || status === 201) {
        show("stored " + path + " (" + body.length + " bytes)", false);
      } else {
        show(errorMessage(status, text), true);
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
        show(text, false);
      } else {
        show(errorMessage(status, text), true);
      }
    });
  });
})();
