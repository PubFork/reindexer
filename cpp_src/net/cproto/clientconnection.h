#pragma once

#include <atomic>
#include <condition_variable>
#include <thread>
#include <vector>
#include "args.h"
#include "cproto.h"
#include "estl/atomic_unique_ptr.h"
#include "estl/h_vector.h"
#include "net/connection.h"
#include "urlparser/urlparser.h"

namespace reindexer {
namespace net {
namespace cproto {

using std::vector;
using std::chrono::seconds;

class ClientConnection;

class RPCAnswer {
public:
	Error Status() const;
	Args GetArgs(int minArgs = 0) const;
	RPCAnswer(const Error &error) : status_(error) {}
	~RPCAnswer() {
		if (hold_) delete[] data_.data();
	}
	RPCAnswer(const RPCAnswer &other) = delete;
	RPCAnswer(RPCAnswer &&other) : status_(std::move(other.status_)), data_(std::move(other.data_)), hold_(std::move(other.hold_)) {
		other.hold_ = false;
	}
	RPCAnswer &operator=(RPCAnswer &&other) {
		if (this != &other) {
			if (hold_) delete[] data_.data();
			status_ = std::move(other.status_);
			data_ = std::move(other.data_);
			hold_ = std::move(other.hold_);
			other.hold_ = false;
		}
		return *this;
	}
	RPCAnswer &operator=(const RPCAnswer &other) = delete;
	void EnsureHold() {
		if (!hold_) {
			uint8_t *data = new uint8_t[data_.size()];
			memcpy(data, data_.data(), data_.size());
			data_ = {data, data_.size()};
			hold_ = true;
		}
	}

protected:
	RPCAnswer() {}
	Error status_;
	span<uint8_t> data_;
	bool hold_ = false;
	friend class ClientConnection;
};

class ClientConnection : public ConnectionMT {
public:
	ClientConnection(ev::dynamic_loop &loop, const httpparser::UrlParser *uri, seconds loginTimeout = seconds(0),
					 seconds requestTimeout = seconds(0));
	~ClientConnection();
	typedef std::function<void(RPCAnswer &&ans, ClientConnection *conn)> Completion;

	template <typename... Argss>
	void Call(const Completion &cmpl, CmdCode cmd, seconds timeout, Argss... argss) {
		Args args;
		args.reserve(sizeof...(argss));
		call(cmpl, cmd, timeout, args, argss...);
	}

	template <typename... Argss>
	RPCAnswer Call(CmdCode cmd, seconds timeout, Argss... argss) {
		Args args;
		args.reserve(sizeof...(argss));

		RPCAnswer ret;
		std::atomic_bool set(false);
		call(
			[&ret, &set](RPCAnswer &&ans, ClientConnection * /*conn*/) {
				ret = std::move(ans);
				ret.EnsureHold();
				set = true;
			},
			cmd, timeout, args, argss...);
		std::unique_lock<std::mutex> lck(mtx_);
		bufWait_++;
		while (!set) {
			bufCond_.wait(lck);
		}
		bufWait_--;

		return ret;
	}

	int PendingCompletions();
	void SetTerminateFlag() noexcept { terminate_.store(true, std::memory_order_release); }
	void SetUpdatesHandler(Completion handler) { updatesHandler_ = handler; }
	seconds Now() const { return seconds(now_); }

protected:
	void connect_async_cb(ev::async &) { connectInternal(); }
	void keep_alive_cb(ev::periodic &, int) {
		if (!terminate_.load(std::memory_order_acquire)) {
			call([](RPCAnswer &&, ClientConnection *) {}, kCmdPing, keepAliveTimeout_, {});
			callback(io_, ev::WRITE);
		}
	}
	void deadline_check_cb(ev::timer &, int);

	void connectInternal();
	void failInternal(const Error &error);

	template <typename... Argss>
	inline void call(const Completion &cmpl, CmdCode cmd, seconds timeout, Args &args, const string_view &val, Argss... argss) {
		args.push_back(Variant(p_string(&val)));
		return call(cmpl, cmd, timeout, args, argss...);
	}
	template <typename... Argss>
	inline void call(const Completion &cmpl, CmdCode cmd, seconds timeout, Args &args, const string &val, Argss... argss) {
		args.push_back(Variant(p_string(&val)));
		return call(cmpl, cmd, timeout, args, argss...);
	}
	template <typename T, typename... Argss>
	inline void call(const Completion &cmpl, CmdCode cmd, seconds timeout, Args &args, const T &val, Argss... argss) {
		args.push_back(Variant(val));
		return call(cmpl, cmd, timeout, args, argss...);
	}

	void call(Completion cmpl, CmdCode cmd, seconds timeout, const Args &args);

	chunk packRPC(CmdCode cmd, uint32_t seq, const Args &args);

	void onRead() override;
	void onClose() override;

	struct RPCCompletion {
		RPCCompletion() : next(nullptr), used(false), deadline(0) {}
		CmdCode cmd;
		uint32_t seq;
		Completion cmpl;
		atomic_unique_ptr<RPCCompletion> next;
		std::atomic<bool> used;
		seconds deadline;
	};

	enum State { ConnInit, ConnConnecting, ConnConnected, ConnFailed };

	State state_;
	// Lock free map seq -> completion
	vector<RPCCompletion> completions_;
	std::condition_variable connectCond_, bufCond_;
	std::atomic<uint32_t> seq_;
	std::atomic<int32_t> bufWait_;
	std::mutex mtx_;
	std::thread::id loopThreadID_;
	Error lastError_;
	const httpparser::UrlParser *uri_;
	ev::async connect_async_;
	Completion updatesHandler_;
	ev::periodic keep_alive_;
	ev::periodic deadlineTimer_;
	std::atomic<uint32_t> now_;
	const seconds loginTimeout_;
	const seconds keepAliveTimeout_;
	std::atomic<bool> terminate_;
};
}  // namespace cproto
}  // namespace net
}  // namespace reindexer
