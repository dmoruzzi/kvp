(function () {
  "use strict";

  var keyEl = document.getElementById("key");
  var valueEl = document.getElementById("value");
  var resultEl = document.getElementById("result");

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
        var msg = "stored";
        try {
          var parsed = JSON.parse(text);
          if (parsed && parsed.message) {
            msg = parsed.message;
          }
        } catch (e) {
          msg = text;
        }
        show(renderStatus(status) + ": " + msg, true);
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
        var msg = text;
        try {
          var parsed = JSON.parse(text);
          if (parsed && parsed.message) {
            msg = parsed.message;
          }
        } catch (e) {
          msg = text;
        }
        show(renderStatus(status) + ": " + msg, true);
      }
    });
  });
})();
