// Offers the browser's time zone as a one-click choice on the setup form.
(function () {
  "use strict";
  var button = document.getElementById("use-browser-tz");
  var select = document.getElementById("timezone");
  if (!button || !select) {
    return;
  }
  var zone;
  try {
    zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  } catch (e) {
    return;
  }
  var known = Array.prototype.some.call(select.options, function (o) {
    return o.value === zone;
  });
  if (!zone || !known) {
    return;
  }
  button.hidden = false;
  button.addEventListener("click", function () {
    select.value = zone;
  });
})();
