// Closes an open drop-down menu (a <details data-menu>) when the pointer
// goes down outside it or Escape is pressed. The menu opens and closes by
// itself without this; the script only adds the dismissal people expect.
(function () {
  "use strict";

  function openMenus() {
    return Array.prototype.filter.call(document.querySelectorAll("details[data-menu]"), function (menu) {
      return menu.open;
    });
  }

  document.addEventListener("pointerdown", function (event) {
    openMenus().forEach(function (menu) {
      if (!menu.contains(event.target)) {
        menu.open = false;
      }
    });
  });

  document.addEventListener("keydown", function (event) {
    if (event.key !== "Escape") {
      return;
    }
    openMenus().forEach(function (menu) {
      menu.open = false;
      var button = menu.querySelector("summary");
      if (button) {
        button.focus();
      }
    });
  });
})();
