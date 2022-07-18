((routes) =>
  routes({
    dc: {
      services: {
        _options: { path: "/services" },
        index: {
          _options: {
            path: "/",
            queryParams: {
              sortBy: "sort",
              status: "status",
              source: "source",
              kind: "kind",
              searchproperty: {
                as: "searchproperty",
                empty: ["PeerName"],
              },
              search: {
                as: "filter",
                replace: true,
              },
            },
          },
        },
      },
      nodes: {
        _options: { path: "/nodes" },
        index: {
          _options: {
            path: "",
            queryParams: {
              sortBy: "sort",
              status: "status",
              searchproperty: {
                as: "searchproperty",
                empty: ["PeerName"],
              },
              search: {
                as: "filter",
                replace: true,
              },
            },
          },
        },
      },
      peers: {
        _options: {
          path: "/peers",
        },
        index: {
          _options: {
            path: "/",
            queryParams: {
              sortBy: "sort",
              state: "state",
              searchproperty: {
                as: "searchproperty",
                empty: ["Name"],
              },
              search: {
                as: "filter",
                replace: true,
              },
            },
          },
        },
        edit: {
          _options: {
            path: "/:name",
          },
          addresses: {
            _options: {
              path: "/addresses",
            },
          },
        },
      },
    },
  }))(
  (
    json,
    data = typeof document !== "undefined"
      ? document.currentScript.dataset
      : module.exports
  ) => {
    data[`routes`] = JSON.stringify(json);
  }
);
