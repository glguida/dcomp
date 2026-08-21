/* Shared viewer state. One mutable object, owned by main.js's fetch loop and
   read by the render layer; selection is {kind: "component"|"link"|"port"}. */
export const state = {
  systems: [],
  current: null,
  doc: null,
  rendered: "",
  selection: null,
  lost: false,
  lastSeen: 0,
  viewBox: null,
  dragged: false,
};
