// Loaded into the exported fixture by TestWriteThreadFixture. No dependencies.
(async function () {
  var result = document.createElement("pre");
  result.id = "browser-test-result";
  document.body.appendChild(result);
  function assert(ok, message) { if (!ok) throw new Error(message); }
  function change(node, event) { node.dispatchEvent(new Event(event || "change")); }
  try {
    var view = document.getElementById("view");
    var query = document.getElementById("q");
    view.value = "threads";
    change(view);
    var threads = Array.from(document.querySelectorAll("details.thread"));
    assert(threads.length === 2, "thread grouping");
    assert(document.getElementById("messages").hidden, "hide message table");
    query.value = "verification";
    change(query, "input");
    var matches = threads.filter(function (t) { return !t.hidden; });
    assert(matches.length === 1, "search threads");
    var thread = matches[0];
    thread.open = true;
    change(thread, "toggle");
    var messages = thread.querySelectorAll("article.message");
    assert(messages.length === 2, "search retains full conversation");
    assert(messages[0].textContent.includes("first reply"), "oldest first");
    assert(messages[0].classList.contains("sent"), "sent direction");
    assert(messages[1].textContent.includes("Received"), "received direction");
    assert(messages[0].textContent.includes("From: You · To: Kalela (+260977000111)"), "outgoing participants");
    assert(messages[1].textContent.includes("From: Kalela (+260977000111) · To: You"), "incoming participants");
    assert(!thread.querySelector("img") && !window.__pwned, "hostile SMS stays text");
    var blob, filename;
    URL.createObjectURL = function (value) { blob = value; return "blob:test"; };
    URL.revokeObjectURL = function () {};
    HTMLAnchorElement.prototype.click = function () { filename = this.download; };
    thread.querySelector("button").click();
    var json = await blob.text();
    var exported = JSON.parse(json);
    assert(exported.length === 2, "download only this thread");
    assert(exported[0].body === "first reply", "download order");
    assert(exported[0].sender === "You" && exported[0].receiver === "Kalela (+260977000111)", "download outgoing participants");
    assert(exported[1].sender === "Kalela (+260977000111)" && exported[1].receiver === "You", "download incoming participants");
    assert(json.includes('"id":9007199254740993'), "download preserves large IDs");
    assert(filename === "sms-thread-message-1.json", "download name");
    query.value = "absent";
    change(query, "input");
    assert(threads.every(function (t) { return t.hidden; }), "no matching threads");
    assert(!document.getElementById("none").hidden, "empty state");
    query.value = "";
    change(query, "input");
    document.querySelector('.chip[data-cat="otp"]').click();
    assert(threads.filter(function (t) { return !t.hidden; }).length === 1, "category filters threads");
    assert(thread.querySelectorAll("article").length === 2, "category retains replies");
    document.getElementById("unsure").checked = true;
    change(document.getElementById("unsure"));
    assert(!thread.hidden, "confidence filter finds thread");
    view.value = "messages";
    change(view);
    assert(!document.getElementById("messages").hidden, "return to messages");
    assert(document.getElementById("threads").hidden, "hide threads");
    assert(document.querySelectorAll("tbody tr:not([hidden])").length === 1, "message filters preserved");
    result.textContent = "PASS: thread browser checks";
  } catch (error) {
    result.textContent = "FAIL: " + error.stack;
  }
})();
