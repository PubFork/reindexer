#include "jsonbuilder.h"

namespace reindexer {

JsonBuilder::JsonBuilder(WrSerializer &ser, ObjType type, const TagsMatcher *tm) : ser_(&ser), tm_(tm), type_(type) {
	switch (type_) {
		case TypeArray:
			(*ser_) << '[';
			break;
		case TypeObject:
			(*ser_) << '{';
			break;
		default:
			break;
	}
}

JsonBuilder::~JsonBuilder() { End(); }

const char *JsonBuilder::getNameByTag(int tagName) { return tagName ? tm_->tag2name(tagName).c_str() : nullptr; }

JsonBuilder &JsonBuilder::End() {
	switch (type_) {
		case TypeArray:
			(*ser_) << ']';
			break;
		case TypeObject:
			(*ser_) << '}';
			break;
		default:
			break;
	}
	type_ = TypePlain;

	return *this;
}

void JsonBuilder::SetTagsMatcher(const TagsMatcher *tm) { tm_ = tm; }

JsonBuilder JsonBuilder::Object(string_view name) {
	putName(name);
	return JsonBuilder(*ser_, TypeObject, tm_);
}

JsonBuilder JsonBuilder::Array(string_view name) {
	putName(name);
	return JsonBuilder(*ser_, TypeArray, tm_);
}

void JsonBuilder::putName(string_view name) {
	if (count_++) (*ser_) << ',';
	if (name.data()) {
		ser_->PrintJsonString(name);
		(*ser_) << ':';
	}
}

JsonBuilder &JsonBuilder::Put(string_view name, string_view arg) {
	putName(name);
	ser_->PrintJsonString(arg);
	return *this;
}

JsonBuilder &JsonBuilder::Raw(string_view name, string_view arg) {
	putName(name);
	(*ser_) << arg;
	return *this;
}
JsonBuilder &JsonBuilder::Null(string_view name) {
	putName(name);
	(*ser_) << "null";
	return *this;
}

JsonBuilder &JsonBuilder::Put(string_view name, const Variant &kv) {
	switch (kv.Type()) {
		case KeyValueInt:
			return Put(name, int(kv));
		case KeyValueInt64:
			return Put(name, int64_t(kv));
		case KeyValueDouble:
			return Put(name, double(kv));
		case KeyValueString:
			return Put(name, string_view(kv));
		case KeyValueNull:
			return Null(name);
		case KeyValueBool:
			return Put(name, bool(kv));
		case KeyValueTuple: {
			auto arrNode = Array(name);
			for (auto &val : kv.getCompositeValues()) {
				arrNode.Put(nullptr, val);
			}
			return *this;
		}
		default:
			break;
	}
	return *this;
}

}  // namespace reindexer
